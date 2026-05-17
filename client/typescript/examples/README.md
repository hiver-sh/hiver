# Examples

## Claude Agent: Expert Quantitative Trade

The agent is given the Swagger spec to a financial data provider,
then uses the sandbox to discover endpoints, make requests, generate to build models, and responde to the user.

The Hive sandbox keeps the API token secure and files persisted to a network file system.

Run with: `ANTHROPIC_API_KEY='<token>' FINNHUB_API_KEY='<token>' npx tsx client/typescript/examples/claude-agent.ts`

```txt
Expert Quantitative Trader
Build financial models, run regressions, design factor strategies, and explain your results in plain language a portfolio manager can act on.

Example Prompts
* Compare the performance of google and nvidia over the last 12 months

you> Compare the performance of google and nvidia over the last 12 months

→ mcp__sandbox__bash {"cmd":"ls /workspace/ && test -f /workspace/swagger.json && echo \"swagger exists\""}
→ mcp__sandbox__bash {"cmd":"mkdir -p /workspace && ls -la /workspace/"}
→ mcp__sandbox__bash {"cmd":"curl -s -o /workspace/swagger.json https://finnhub.io/static/swagger.json && ls -la /work…
→ mcp__sandbox__bash {"cmd":"jq '.paths | keys[]' /workspace/swagger.json | head -50"}
→ mcp__sandbox__bash {"cmd":"jq '.paths.\"/stock/candle\"' /workspace/swagger.json"}
→ mcp__sandbox__bash {"cmd":"jq '.paths | keys[]' /workspace/swagger.json | grep -i -E \"stock|quote|metric|price\""}
→ mcp__sandbox__bash {"cmd":"jq '.paths.\"/stock/price-metric\"' /workspace/swagger.json | head -40\necho \"---\"\njq …
→ mcp__sandbox__bash {"cmd":"# Check the quote endpoint, this is free\ncurl -s -G \"https://finnhub.io/api/v1/quote\" …
→ mcp__sandbox__bash {"cmd":"curl -s -G \"https://finnhub.io/api/v1/stock/metric\" \\\n  --data-urlencode \"symbol=GOO…
→ mcp__sandbox__bash {"cmd":"jq '.metric | to_entries[] | select(.key|test(\"52|Return|Performance|YTD|beta|Volatility…
→ mcp__sandbox__bash {"cmd":"jq '.metric | to_entries[] | select(.key|test(\"beta|Volatility\";\"i\")) | \"\\(.key): \…
→ mcp__sandbox__bash {"cmd":"jq '.metric | to_entries[] | select(.key|test(\"revenueGrowth|grossMargin|netProfit|roe|r…
→ mcp__sandbox__bash {"cmd":"# Try to backfill historical price one year ago using the metric - we have 52W return so …
→ mcp__sandbox__write {"path":"/workspace/goog_nvda/report.md","content":"# GOOGL vs NVDA — 12-Month Performance Compar…

Comparison of 📈 **GOOGL** vs 📈 **NVDA** over the trailing 12 months (as of 2026-05-17, last close 2026-05-16):

Headline returns 🏁

Window              GOOGL       NVDA     Winner
52-week return    +142.0 %    +67.1 %    🟢 GOOGL  (~75 pp gap)
6-month (26W)      +42.3 %    +19.8 %    🟢 GOOGL
3-month (13W)      +28.4 %    +20.5 %    🟢 GOOGL
YTD 2026           +26.8 %    +20.8 %    🟢 GOOGL
Excess vs S&P 500 +116.8 %    +41.9 %    🟢 GOOGL

GOOGL implied price ~12 months ago: ≈ $164 → $396.8 today.
NVDA  implied price ~12 months ago: ≈ $135 → $225.3 today.
Both made fresh 52-week highs this week (GOOGL 2026-05-13 at $403.7, NVDA 2026-05-14 at $236.5).

Risk profile ⚖️
              GOOGL    NVDA
3M annualised vol      34.3 %   37.7 %
Beta (5Y monthly)       1.27     2.26
Return / vol (52W)      4.14     1.78

GOOGL delivered ~2× the absolute return with **lower vol and roughly half the beta** — risk-adjusted, the dominance is even larger.

Valuation 💰
                  GOOGL     NVDA
Market cap        $4.80 T   $5.48 T   (NVDA is still the biggest cap)
P/E TTM           30.0      45.6
Forward P/E       28.6      22.0      ⬅ NVDA cheaper on forward
PEG TTM           1.73      1.03      ⬅ NVDA cheaper on growth

Fundamentals (TTM) 🧪
                  GOOGL     NVDA
Revenue growth    +17.5 %   +65.5 %
EPS growth YoY    +48.5 %   +66.8 %
Gross margin      60.4 %    71.2 %
Operating margin  32.7 %    60.4 %
Net margin        37.9 %    55.6 %
ROE / ROA         39 / 27 % 104 / 76 %
NVDA still prints the better growth and margin profile; GOOGL’s engine is excellent but materially behind on growth.

Bottom line for a PM 🧭
- 🥇 Past-12M winner: **GOOGL** — bigger return, lower vol, lower beta, *and* a cheaper forward multiple than NVDA.
- 🚀 Forward growth/PEG winner: **NVDA** — margins, ROE >100 %, PEG ~1.0; the market is still paying up for AI capex growth.
- 🔁 Pair-trade implication: after GOOGL’s big re-rating, long-NVDA / short-GOOGL is the cheaper-on-forward-PE basket today, but it doubles your beta exposure (~2.3) and concentrates you on hyperscaler capex.
- ⚠️ Caveats: numbers are derived from vendor summary statistics (daily candle data wasn’t accessible on this tier), so intraday/drawdown stats are approximations. Returns are price-only — GOOGL’s dividend yield (<0.25 %) makes the total-return picture essentially identical.

Full write-up saved to `/workspace/goog_nvda/report.md` and raw metric snapshots to `/workspace/goog_nvda/googl_metric.json` and `/workspace/goog_nvda/nvda_metric.json`. 🗂️
```