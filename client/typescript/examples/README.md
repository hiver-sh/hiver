# Examples

## Claude Agent: Expert Quantitative Trade

The agent uses a Swagger spec to discover endpoints, then the sandbox uses CURL to fetch data, Python to build financial models, and Google drive to persist files over a fuse mount.

The Hive Sandbox keeps the API tokens secure and generated files persisted to Google drive all while using basic Bash commands.
The next time this agent runs, all the files are available, so they can be re-used to save tokens and increase learnings.

For example, the agent can store markdown, json files and use them next time this agent runs.

Run with: `ANTHROPIC_API_KEY='<token>' FINNHUB_API_KEY='<token>' GOOGLE_CLIENT_ID='<client-id>' GOOGLE_CLIENT_SECRET='<client-secret>' npx tsx client/typescript/examples/claude-agent-gdrive-filesystem.ts`

```txt

you> Compare the performance of google and nvidia over the last 12 months

→ mcp__sandbox__bash {"cmd":"ls /workspace/ 2>&1 | head -20"}
→ mcp__sandbox__bash {"cmd":"ls -la /workspace/"}
→ mcp__sandbox__bash {"cmd":"curl -s -o /workspace/swagger.json https://finnhub.io/static/swagger.json && ls -la /work…
→ mcp__sandbox__bash {"cmd":"jq '.paths | keys' /workspace/swagger.json | head -60"}
→ mcp__sandbox__bash {"cmd":"jq '.paths.\"/stock/candle\"' /workspace/swagger.json | head -50"}
→ mcp__sandbox__bash {"cmd":"jq '.paths | keys[]' /workspace/swagger.json | grep -i -E \"stock|price|metric|quote\""}
→ mcp__sandbox__bash {"cmd":"jq '.paths.\"/stock/price-metric\"' /workspace/swagger.json | head -30\necho \"---\"\njq …
→ mcp__sandbox__bash {"cmd":"jq '.paths.\"/stock/historical-market-cap\"' /workspace/swagger.json | head -20\necho \"-…
→ mcp__sandbox__bash {"cmd":"mkdir -p /workspace/compare\n# Get quote and basic financials for both stocks\nfor sym in…
→ mcp__sandbox__bash {"cmd":"cat /workspace/compare/GOOGL_quote.json | python3 -m json.tool\necho \"---\"\ncat /worksp…
→ mcp__sandbox__bash {"cmd":"jq '.metric | with_entries(select(.key | test(\"52|Return|Beta|volatility|Volatility|Dail…
→ mcp__sandbox__bash {"cmd":"jq '.metric | with_entries(select(.key | test(\"52|Return|Beta|volatility|Volatility|Dail…
→ mcp__sandbox__bash {"cmd":"jq '.metric | with_entries(select(.key | test(\"marketCap|pe|peNorm|pbAnnual|psAnnual|cur…
→ mcp__sandbox__bash {"cmd":"pip install --quiet matplotlib pandas numpy 2>&1 | tail -3"}
→ mcp__sandbox__bash {"cmd":"python3 -c \"import matplotlib, pandas, numpy; print('ok')\""}
→ mcp__sandbox__bash {"cmd":"pip install --quiet --break-system-packages matplotlib pandas numpy 2>&1 | tail -3"}
→ mcp__sandbox__write {"path":"/workspace/compare/analyze.py","content":"#!/usr/bin/env python3\n\"\"\"Compare GOOGL vs…
→ mcp__sandbox__bash {"cmd":"python3 /workspace/compare/analyze.py"}
→ mcp__sandbox__write {"path":"/workspace/compare/report.md","content":"# GOOGL vs NVDA — Trailing 12-Month Performance…

Here is the side-by-side over the last 12 months (as of 2026-05-17) — pulled from cached Finnhub Basic Financials and the latest quote 📊

Metric                                       GOOGL           NVDA
-----------------------------------------------------------------
Current price ($)                           396.80         225.34
52-week high ($)                            403.70         236.54
52-week low ($)                             162.00         129.16
52-week return (%)                          142.00          67.11
26-week return (%)                           42.30          19.76
13-week return (%)                           28.41          20.53
YTD return (%)                               26.77          20.82
Rel. to S&P 500 52w (pp)                    116.81          41.93
3M annualised vol (%)                        34.33          37.71
Beta (vs S&P 500)                             1.27           2.26
Return/Vol ratio (12m)                        4.14           1.78
Drawdown from 52w high (%)                   -1.71          -4.73
Market cap ($ M)                      4,799,847.50   5,475,276.00
TTM P/E                                      29.96          45.60
Forward P/E                                  28.60          22.05
P/S (annual)                                 11.92          25.36
PEG (TTM)                                     1.73           1.03
Gross margin TTM (%)                         60.37          71.23
Operating margin TTM (%)                     32.69          60.38
ROE (RFY) (%)                                31.83          76.33
Revenue growth YoY TTM (%)                   17.45          65.47
EPS growth YoY TTM (%)                       48.51          66.76

🏆 Price performance over the trailing 12 months
- GOOGL +142.0% vs NVDA +67.1%. Google more than doubled NVIDIA's return and beat the S&P 500 by ~117 pp; NVIDIA beat it by ~42 pp.
- The gap is consistent across windows: GOOGL also wins 26w (+42% vs +20%), 13w (+28% vs +21%) and YTD (+27% vs +21%).
- Both names are within ~5% of their 52-week high — momentum is alive on both sides — but GOOGL is the more "extended" of the two.

⚖️ Risk-adjusted
- 12-month return / 3-month annualised vol: **GOOGL 4.14 vs NVDA 1.78** — Google delivered a much better return per unit of risk.
- GOOGL beta 1.27 vs NVDA 2.26 — NVDA still moves roughly 2x the index, so it carried more market-risk to earn the smaller return.

🏭 Fundamentals (still in NVDA's favor)
- NVDA: gross margin 71%, op margin 60%, ROE 76%, revenue +65% YoY, EPS +67% YoY.
- GOOGL: gross 60%, op 33%, ROE 32%, revenue +17% YoY, EPS +49% YoY.
- NVIDIA is unambiguously the higher-quality compounder; Google is the more diversified, lower-volatility business.

💰 Valuation
- TTM P/E: GOOGL 30 vs NVDA 46 — NVDA is richer on trailing earnings.
- Forward P/E: GOOGL 28.6 vs NVDA 22.0 — analyst forecasts make NVDA optically cheaper on forward earnings.
- PEG TTM: NVDA 1.03 vs GOOGL 1.73 — growth-adjusted, NVDA is the cheaper of the two.

🎯 PM takeaway
- Pure 12-month tape: **GOOGL won**, and on a risk-adjusted basis it wasn't close.
- NVDA's underperformance is multiple compression after its 2024-25 megarun, not earnings deterioration — the fundamentals are actually getting better.
- If you're momentum/quality-of-tape oriented, stay long GOOGL; if you're rotating into growth-at-a-reasonable-price (PEG ~1) with a 12-month horizon, NVDA looks like the more interesting set-up from here.

Full write-up saved to /workspace/compare/report.md; raw data and the analysis script are in /workspace/compare/.
```

If you prefer to use the local file system, use the `claude-agent.ts` example:
`ANTHROPIC_API_KEY='<token>' FINNHUB_API_KEY='<token>' npx tsx client/typescript/examples/claude-agent.ts`
