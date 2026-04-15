# GoClaw 개발 노트

개발 중 주요 결정사항, 아키텍처, 연동 구조를 기록합니다.

---

## KIS 전략빌더 · 백테스터 연동

### 시스템 역할 분리

| 시스템 | 역할 |
|--------|------|
| `KIS strategy builder` | 수동 종목 선정 및 전략 실행을 위한 **UI + 기능 코드** |
| `KIS backtester` | 전략 백테스트 **UI + 기능 코드** |
| **goclaw 에이전트** | 위 두 시스템의 코드를 **직접 import**해서 자동화 실행 |

goclaw는 KIS strategy builder / backtester를 REST API가 아닌 **Python 직접 import** 방식으로 활용한다.  
따라서 goclaw 실행 환경의 `sys.path`에 KIS strategy builder 경로가 포함되어야 한다.

### KIS strategy builder 주요 모듈 (import 대상)

```
/home/user/KIS/open-trading-api/strategy_builder/
├── core/
│   ├── data_fetcher.py      # KIS API 호출 (일봉, 현재가, 잔고 등)
│   ├── indicators.py        # 100+ 기술적 지표 계산
│   ├── signal.py            # Signal / Action 클래스
│   ├── market_schedule.py   # TradingSession, NXT 스케줄
│   └── order_executor.py    # 주문 실행
├── strategy/
│   ├── base_strategy.py     # BaseStrategy 추상 클래스
│   └── strategy_01~100.py   # 100개 전략 구현체
├── strategy_core/
│   ├── registry.py          # StrategyRegistry
│   └── preset/              # 126개 프리셋 전략 (자동 등록)
└── agent/                   # 자율 에이전트 (goclaw가 활용)
    ├── regime_detector.py   # 시장 국면 탐지 (KOSPI ETF 기반)
    ├── scorecard.py         # 국면 × 전략 적합도 점수
    ├── universe.py          # 종목 유니버스 관리
    └── autonomous_agent.py  # 전체 스캔 사이클
```

### 마스터파일

**저장 형식: JSON** (`.master/{exchange}.json`) — CSV에서 교체 완료

| 파일 | 내용 | 레코드 크기 |
|------|------|-----------|
| `kospi.json` | 코스피 전체 (~2,496종) + nxt_enabled 플래그 | 288 bytes 전체 파싱 |
| `kosdaq.json` | 코스닥 전체 (~1,826종) + nxt_enabled 플래그 | 288 bytes 전체 파싱 |
| `nxt_kospi.json` | NXT 대상 코스피 (~359종) | 동일 구조 |
| `nxt_kosdaq.json` | NXT 대상 코스닥 (~71종 추정) | 동일 구조 |
| `nxt_codes.json` | `{"kospi": [...], "kosdaq": [...]}` | 빠른 집합 조회용 |
| `konex.json` | 코넥스 | 단순(코드+이름) |
| `elw.json` | ELW | 단순 |
| `idxcode.json` | 업종코드 | 단순 |
| `theme.json` | 테마코드 | 단순 |
| `bond.json` | 장내채권 | 단순 |

**파싱 필드 (kospi/kosdaq/nxt_*):**
- `code`, `name`, `exchange`
- `market_cap_size`: 1=대형, 2=중형, 3=소형, 0=미분류
- `is_kospi100`, `is_kospi50`, `is_krx300` (KOSPI only)
- `is_halted`: 거래정지 여부
- `is_managed`: 관리종목 여부
- `listed_date`: 상장일 (YYYYMMDD)
- `prev_mktcap_bil`: 전일 시가총액(억원)
- `nxt_enabled`: NXT 거래 가능 (마스터파일 기반, API 호출 0회)

**수집 API:**
- `POST /api/symbols/collect` — 기본: 핵심 4개 (kospi/kosdaq/nxt_*)
- `POST /api/symbols/collect` body `{"exchanges": ["all"]}` — 전체 9종 수집
- **자동 수집 스케줄은 goclaw가 담당** (매 영업일 07:30)

**수집 중앙 모듈:** `core/master_loader.py`
```python
from core import master_loader

# 핵심 4개 수집
master_loader.collect()

# 전체 수집
master_loader.collect_all()

# NXT 코드 집합 조회 (API 호출 없음)
nxt_set = master_loader.get_nxt_codes("kospi")  # → set[str]

# JSON 로드
records = master_loader.load_json("kospi")  # → list[dict]
```

### NXT(넥스트트레이딩) 지원

- 운영시간: 프리마켓 08:00~09:00 / 정규장 09:00~15:30 / 시간외 15:30~18:00
- 시장 구분 코드: `NX`(NXT 단독), `UN`(KRX+NXT 통합가격)
- NXT 가능 종목은 `UN` 가격으로 신호 생성
- `TradingSession` enum: `BEFORE_MARKET / NXT_PRE / REGULAR / NXT_AFTER / CLOSED`
- **NXT 적격 여부 판단**: `nxt_kospi_code.mst.zip` / `nxt_kosdaq_code.mst.zip` 파일 등재 여부 (별도 API 불필요)
- `is_nxt_eligible()` 조회 순서: nxt_codes.json → nxt_eligibility.json(레거시) → None(API fallback)

### 자율 에이전트 스캔 사이클

```
detect_regime()          # KOSPI 프록시(069500) 일봉 → 추세/변동성/모멘텀
  ↓
score_strategies()       # 국면 × 카테고리 매트릭스 → 상위 N 전략 선택
  ↓
load_premium_universe()  # KOSPI 50 + KOSDAQ 20 대형주 + 세션 필터
  ↓
strategy.generate_signal() × (종목 × 전략)   # 0.2초 rate-limit
  ↓
aggregate()              # 가중합산 + 합의 보너스 → composite_score
  ↓
AgentResult              # buy_signals / sell_signals 정렬
```

---

## 향후 작업 (TODO)

- [ ] goclaw에서 KIS strategy builder 직접 import 연동 코드
- [ ] 마스터파일 자동 수집 스케줄러 (goclaw 쪽 구현, `master_loader.collect()` 호출)
- [ ] 백테스터 연동 (전략 검증 후 실전 적용 파이프라인)
- [x] NXT eligibility — 마스터파일 기반으로 전환 (API 호출 0회)
