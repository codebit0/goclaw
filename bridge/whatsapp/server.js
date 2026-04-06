/**
 * GoClaw WhatsApp Bridge
 *
 * Scope: owns the WhatsApp Baileys lifecycle, QR flow, and WS server.
 * GoClaw connects as a client and drives business logic (routing, pairing, AI).
 *
 * Protocol — Bridge → GoClaw:
 *   { type: "status",       connected: bool }               WhatsApp auth state (sent on connect + on change)
 *   { type: "qr",          data: "<qr-string>" }           QR code for scanning
 *   { type: "message",     id, from, chat, content,        Incoming WhatsApp message
 *                          from_name, is_group,
 *                          media: [{type,mimetype,filename,path}] }
 *   { type: "pong" }                                        Response to ping
 *
 * Protocol — GoClaw → Bridge:
 *   { type: "message",  to: "<jid>", content: "<text>" }             Send outbound text
 *   { type: "media",    to, path, mimetype, caption? }              Send outbound media file
 *   { type: "command",  action: "reauth" }                            Logout + restart QR flow
 *   { type: "command",  action: "ping" }                              Health check
 *   { type: "command",  action: "presence", to, state }              Presence update (composing|paused|available)
 */

import makeWASocket, {
  useMultiFileAuthState,
  DisconnectReason,
  fetchLatestBaileysVersion,
  downloadMediaMessage,
} from '@whiskeysockets/baileys'
import { WebSocketServer } from 'ws'
import { rm, readdir, writeFile, mkdir } from 'node:fs/promises'
import { join, extname } from 'node:path'
import { randomBytes } from 'node:crypto'
import { tmpdir } from 'node:os'
import qrcode from 'qrcode-terminal'
import { Boom } from '@hapi/boom'
import pino from 'pino'

const PORT      = parseInt(process.env.BRIDGE_PORT || '3001', 10)
const AUTH_DIR  = process.env.AUTH_DIR || './auth_info'
const LOG_LEVEL = process.env.LOG_LEVEL || 'silent'  // "debug" for Baileys internals

// Max media download size (20 MB). Files larger than this are skipped.
const MEDIA_MAX_BYTES = parseInt(process.env.MEDIA_MAX_BYTES || String(20 * 1024 * 1024), 10)

// Temp directory for downloaded media files. GoClaw reads from here.
const MEDIA_DIR = process.env.MEDIA_DIR || join(tmpdir(), 'goclaw_wa_media')

// Baileys logger is capped at 'warn' regardless of LOG_LEVEL.
// Baileys logs Signal Protocol session internals (private keys, chain keys, root keys)
// at debug/info level — never surface those even during local debugging.
const logger = pino({ level: LOG_LEVEL === 'silent' ? 'silent' : 'warn' })
const wss    = new WebSocketServer({ port: PORT })

// --- State ---

/** @type {Set<import('ws').WebSocket>} */
const clients = new Set()

/** @type {import('@whiskeysockets/baileys').WASocket | null} */
let sock           = null
let reconnectTimer = null
let waConnected    = false  // true once Baileys reports 'open' for this session
let isReauthing    = false  // prevents 401 from stopping reconnect during reauth
let lastQR         = null   // cached QR string — replayed to clients that reconnect mid-flow
let botJID         = ''     // cached bare bot JID (phone) — replayed to clients that connect after auth
let botLID         = ''     // cached bare bot LID (Link ID) — WhatsApp's newer identifier system

// --- Helpers ---

/** Send JSON to all connected GoClaw clients. */
function broadcast(payload) {
  const data = JSON.stringify(payload)
  for (const ws of clients) {
    if (ws.readyState === 1) ws.send(data)
  }
}

/** Send JSON to a single client. */
function sendTo(ws, payload) {
  if (ws.readyState === 1) ws.send(JSON.stringify(payload))
}

/** Extract plain text from any Baileys message variant. */
function extractContent(message) {
  if (!message) return ''
  return (
    message.conversation ||
    message.extendedTextMessage?.text ||
    message.imageMessage?.caption ||
    message.videoMessage?.caption ||
    message.documentMessage?.caption ||
    message.buttonsResponseMessage?.selectedDisplayText ||
    message.listResponseMessage?.title ||
    ''
  )
}

/** MIME → file extension mapping for downloaded media. */
const mimeToExt = {
  'image/jpeg': '.jpg', 'image/png': '.png', 'image/webp': '.webp', 'image/gif': '.gif',
  'video/mp4': '.mp4', 'video/3gpp': '.3gp',
  'audio/ogg; codecs=opus': '.ogg', 'audio/mpeg': '.mp3', 'audio/mp4': '.m4a', 'audio/aac': '.aac',
  'application/pdf': '.pdf',
}

/** Map a Baileys message to a list of media descriptors (type, mimetype, filename, messageKey). */
function detectMedia(message) {
  if (!message) return []
  const items = []

  if (message.imageMessage)    items.push({ type: 'image',    mimetype: message.imageMessage.mimetype,    messageKey: 'imageMessage' })
  if (message.videoMessage)    items.push({ type: 'video',    mimetype: message.videoMessage.mimetype,    messageKey: 'videoMessage' })
  if (message.audioMessage)    items.push({ type: 'audio',    mimetype: message.audioMessage.mimetype,    messageKey: 'audioMessage' })
  if (message.documentMessage) items.push({ type: 'document', mimetype: message.documentMessage.mimetype, messageKey: 'documentMessage',
                                             filename: message.documentMessage.fileName })
  if (message.stickerMessage)  items.push({ type: 'sticker',  mimetype: message.stickerMessage.mimetype,  messageKey: 'stickerMessage' })
  return items
}

/**
 * Download media from a Baileys message and save to temp files.
 * Returns array of { type, mimetype, filename, path } for successfully downloaded items.
 */
async function downloadMedia(msg) {
  const items = detectMedia(msg.message)
  if (items.length === 0) return []
  if (!sock) return []

  await mkdir(MEDIA_DIR, { recursive: true })

  const results = []
  for (const item of items) {
    try {
      // downloadMediaMessage needs the full msg (with key + message), not just msg.message.
      // 4th arg: reuploadRequest must be bound to sock to preserve `this` context.
      const buffer = await downloadMediaMessage(msg, 'buffer', {}, {
        logger,
        reuploadRequest: sock.updateMediaMessage?.bind(sock),
      })
      if (!buffer || buffer.length === 0) {
        console.warn(`⚠️  Empty buffer for ${item.type}, skipping`)
        continue
      }
      if (buffer.length > MEDIA_MAX_BYTES) {
        console.warn(`⚠️  Media too large (${(buffer.length / 1024 / 1024).toFixed(1)} MB), skipping`)
        continue
      }
      const ext = mimeToExt[item.mimetype] || extname(item.filename || '') || '.bin'
      const name = `goclaw_wa_${randomBytes(8).toString('hex')}${ext}`
      const filePath = join(MEDIA_DIR, name)
      await writeFile(filePath, buffer)

      results.push({
        type:     item.type,
        mimetype: item.mimetype,
        filename: item.filename || '',
        path:     filePath,
      })
      console.log(`📎 Downloaded ${item.type} (${(buffer.length / 1024).toFixed(0)} KB) → ${name}`)
    } catch (err) {
      console.error(`❌ Media download failed (${item.type}):`, err.message)
    }
  }
  return results
}

// --- Baileys lifecycle ---

async function connectToWhatsApp() {
  if (reconnectTimer) {
    clearTimeout(reconnectTimer)
    reconnectTimer = null
  }

  const { state, saveCreds } = await useMultiFileAuthState(AUTH_DIR)

  const { version } = await fetchLatestBaileysVersion().catch(() => ({
    version: [2, 3000, 1023456789],
  }))

  sock = makeWASocket({
    version,
    auth: state,
    logger,
    printQRInTerminal: false,
    browser: ['GoClaw Bridge', 'Chrome', '1.0.0'],
    keepAliveIntervalMs: 30_000,
  })

  sock.ev.on('creds.update', saveCreds)

  sock.ev.on('connection.update', ({ connection, lastDisconnect, qr }) => {
    if (qr) {
      lastQR = qr  // cache so reconnecting GoClaw clients don't miss it
      broadcast({ type: 'qr', data: qr })
      // Print QR to terminal only when PRINT_QR=true (useful without a UI)
      if (process.env.PRINT_QR === 'true') {
        console.log('\n📱 Scan this QR with WhatsApp (You → Linked Devices → Link a Device):')
        qrcode.generate(qr, { small: true })
      }
    }

    if (connection === 'close') {
      const statusCode = new Boom(lastDisconnect?.error)?.output?.statusCode
      const loggedOut = statusCode === DisconnectReason.loggedOut
      console.log(`❌ WhatsApp disconnected (code ${statusCode})`)
      waConnected = false
      broadcast({ type: 'status', connected: false })

      if (!loggedOut || isReauthing) {
        // Normal disconnect → retry; or reauth in progress → reconnect to show new QR.
        isReauthing = false
        reconnectTimer = setTimeout(connectToWhatsApp, loggedOut ? 500 : 5_000)
      } else {
        console.log('🚪 Logged out — send { type:"command", action:"reauth" } to re-pair')
      }
    } else if (connection === 'open') {
      console.log('✅ WhatsApp authenticated!')
      lastQR = null  // clear cached QR — no longer needed
      waConnected = true
      // Include the bot's own JIDs so GoClaw can detect @mentions.
      // WhatsApp uses two identifier systems: phone JID (@s.whatsapp.net) and LID (@lid).
      // Mentions in groups may use either format, so we send both.
      // Strip Baileys device suffix (e.g. "12345:42@s.whatsapp.net" → "12345@s.whatsapp.net")
      const rawJid = sock.user?.id ?? ''
      botJID = rawJid.replace(/:\d+@/, '@')
      const rawLid = sock.user?.lid ?? ''
      botLID = rawLid.replace(/:\d+@/, '@')
      broadcast({ type: 'status', connected: true, me: botJID, me_lid: botLID })
    }
  })

  sock.ev.on('messages.upsert', async ({ messages, type }) => {
    if (type !== 'notify') return  // skip history / append

    for (const msg of messages) {
      if (msg.key.fromMe) continue
      if (!msg.message) continue   // receipts, ephemeral control frames, etc.

      const chatJid   = msg.key.remoteJid
      if (!chatJid) continue

      // Groups: participant = sender JID, remoteJid = group JID
      // DMs:    participant is undefined, remoteJid = sender JID
      const senderJid = msg.key.participant || chatJid
      const content   = extractContent(msg.message)
      const hasMedia  = detectMedia(msg.message).length > 0

      if (!content && !hasMedia) continue  // completely empty, skip

      // Download media files (async). Non-blocking: failures are logged and skipped.
      const media = hasMedia ? await downloadMedia(msg) : []

      // Extract @mentioned JIDs from extended text messages.
      const mentionedJids = msg.message?.extendedTextMessage?.contextInfo?.mentionedJid ?? []

      const payload = {
        type:           'message',
        id:             msg.key.id,
        from:           senderJid,
        chat:           chatJid,
        content:        content,
        from_name:      msg.pushName || '',
        is_group:       chatJid.endsWith('@g.us'),
        mentioned_jids: mentionedJids,
        media,
      }

      console.log(`📨 ${senderJid} → ${chatJid}: ${content.slice(0, 60)}${content.length > 60 ? '…' : ''}`)
      broadcast(payload)
    }
  })
}

/**
 * Force-clear the auth state and restart so a fresh QR is generated.
 * Called when GoClaw sends { type: "command", action: "reauth" }.
 *
 * We do NOT call sock.logout() because it fires connection.update with
 * DisconnectReason.loggedOut (401), which makes shouldReconnect=false and
 * stops the reconnect loop. Instead we forcibly end the socket and delete
 * the auth files so the next connection starts a clean QR flow.
 */
async function handleReauth() {
  console.log('🔄 Reauth requested — clearing session and restarting...')
  isReauthing = true
  waConnected = false
  botJID = ''
  botLID = ''
  broadcast({ type: 'status', connected: false })

  if (reconnectTimer) {
    clearTimeout(reconnectTimer)
    reconnectTimer = null
  }

  if (sock) {
    sock.ev.removeAllListeners()
    try { sock.end(new Error('reauth requested')) } catch { /* ignore */ }
    sock = null
  }

  // Delete all auth files individually (avoids EBUSY on the directory itself).
  // Must clear everything — leaving stale pre-keys or app-state causes
  // WhatsApp to reject the new QR/pairing attempt with "can't link devices".
  try {
    const files = await readdir(AUTH_DIR)
    await Promise.all(files.map(f => rm(join(AUTH_DIR, f), { force: true })))
    console.log(`🗑️  Auth state cleared (${files.length} files)`)
  } catch (err) {
    console.warn('⚠️  Could not clear auth state:', err.message)
  }

  setTimeout(connectToWhatsApp, 500)
}

// --- WS server: handle GoClaw client connections ---

wss.on('connection', ws => {
  console.log('🔌 GoClaw connected')
  clients.add(ws)

  // Send current auth state immediately so GoClaw doesn't have to guess.
  // Include bot JID + LID so mention detection works even if GoClaw connects after auth.
  sendTo(ws, { type: 'status', connected: waConnected, ...(botJID && { me: botJID }), ...(botLID && { me_lid: botLID }) })
  // Replay cached QR so GoClaw doesn't miss it if it reconnected mid-flow.
  if (lastQR && !waConnected) {
    sendTo(ws, { type: 'qr', data: lastQR })
  }

  ws.on('message', async rawData => {
    let msg
    try {
      msg = JSON.parse(rawData.toString())
    } catch {
      console.warn('⚠️  Non-JSON from GoClaw, ignoring')
      return
    }

    if (msg.type === 'command') {
      switch (msg.action) {
        case 'reauth':
          await handleReauth()
          break
        case 'ping':
          sendTo(ws, { type: 'pong' })
          break
        case 'presence': {
          // state: 'composing' | 'paused' | 'available'
          if (!sock || !waConnected) break
          try {
            const to = (msg.to ?? '').replace('@c.us', '@s.whatsapp.net')
            await sock.sendPresenceUpdate(msg.state ?? 'available', to)
          } catch (err) {
            console.error('❌ Presence update error:', err.message)
          }
          break
        }
        default:
          console.warn('⚠️  Unknown command:', msg.action)
      }
      return
    }

    if (msg.type === 'media') {
      if (!msg.to || !msg.path) {
        console.warn('⚠️  Outbound media missing "to" or "path"', msg)
        return
      }
      if (!sock || !waConnected) {
        console.warn('⚠️  WhatsApp not connected — dropping media to', msg.to)
        return
      }
      try {
        const to = msg.to.replace('@c.us', '@s.whatsapp.net')
        const { readFile } = await import('node:fs/promises')
        const buffer = await readFile(msg.path)
        const mime = (msg.mimetype || '').toLowerCase()
        const caption = msg.caption || undefined

        let content
        if (mime.startsWith('image/'))      content = { image: buffer, caption }
        else if (mime.startsWith('video/')) content = { video: buffer, caption }
        else if (mime.startsWith('audio/')) content = { audio: buffer, mimetype: mime }
        else                                content = { document: buffer, mimetype: mime, caption,
                                                        fileName: msg.path.split('/').pop() }

        await sock.sendMessage(to, content)
        console.log(`📤 Sent ${mime || 'media'} to ${to}`)
      } catch (err) {
        console.error('❌ Failed to send WhatsApp media:', err.message)
      }
      return
    }

    if (msg.type === 'message') {
      if (!msg.to || !msg.content) {
        console.warn('⚠️  Outbound message missing "to" or "content"', msg)
        return
      }
      if (!sock || !waConnected) {
        console.warn('⚠️  WhatsApp not connected — dropping reply to', msg.to)
        return
      }
      try {
        // Normalise JID: @c.us (whatsapp-web.js) → @s.whatsapp.net (Baileys)
        const to = msg.to.replace('@c.us', '@s.whatsapp.net')
        await sock.sendMessage(to, { text: msg.content })
        console.log(`📤 Sent to ${to}`)
      } catch (err) {
        console.error('❌ Failed to send WhatsApp message:', err.message)
      }
      return
    }

    console.warn('⚠️  Unknown message type from GoClaw:', msg.type)
  })

  ws.on('close', () => {
    console.log('🔌 GoClaw disconnected')
    clients.delete(ws)
  })

  ws.on('error', err => console.error('WebSocket error:', err.message))
})

wss.on('listening', () => {
  console.log(`🌉 WhatsApp bridge on ws://0.0.0.0:${PORT}  auth=${AUTH_DIR}`)
  connectToWhatsApp().catch(err => {
    console.error('❌ Fatal error connecting to WhatsApp:', err)
    process.exit(1)
  })
})

wss.on('error', err => {
  console.error('❌ WebSocket server error:', err.message)
  process.exit(1)
})
