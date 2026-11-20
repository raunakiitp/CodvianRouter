# B8 — Dashboard features, one by one (for the owner to cut)

Source: `docs/PARITY.md` Area 8b section (lines 843–998, read 2026-11-20). Every line
traces to a PARITY.md line number. Plain words: what you see or do in the dashboard.
Verdicts are the file's current verdicts. Strike any line you do not want built —
nothing in B8 is built until you approve this list.

Count: **141 features** (see the count note at the end).

## The list

1. Pick a routing strategy from a dropdown in the dashboard — PARITY.md:858 — FULL
2. Save named routing combinations, copy them, import/export them, test them, switch them on/off — PARITY.md:859 — MISSING
3. Set different behaviour per capability (chat, search, embeddings, images, video, music, OCR, audio) in the combo builder — PARITY.md:860 — MISSING
4. Pick a default auto-routing variant (10 choices) — PARITY.md:861 — PARTIAL
5. Turn session stickiness on/off (same user keeps going to the same provider) — PARITY.md:862 — MISSING
6. Create a new router API key from the dashboard — PARITY.md:863 — PARTIAL
7. Regenerate an API key, with a confirm dialog — PARITY.md:864 — FULL
8. Add/delete per-provider keys and set their role (Preferred / Reserve) — PARITY.md:865 — FULL
9. Enforce key scopes on routes (a key can only do what its scope allows) — PARITY.md:866 — FULL
10. Disable or revoke one single key (not all keys at once) — PARITY.md:867 — MISSING
11. Daily $ limit per key — PARITY.md:868 — MISSING
12. Weekly $ limit per key — PARITY.md:868 — MISSING
13. Monthly $ limit per key — PARITY.md:868 — MISSING
14. Rolling 5-hour $ budget per key — PARITY.md:869 — FULL
15. Quota-share pools (share quota fairly across keys) — PARITY.md:870 — MISSING
16. Team cost centres (group spend by team) — PARITY.md:871 — MISSING
17. Cost overview numbers: spend 1/7/30 days, requests, active providers/models, average cost per request — PARITY.md:872 — PARTIAL
18. Monthly spend forecast — PARITY.md:873 — MISSING
19. Cost explorer: filter by dimension, top 50, export CSV/JSON — PARITY.md:874 — MISSING
20. Combo health: quota risk, forecast, fairness, autopilot, scoring inspector — PARITY.md:875 — MISSING
21. Request logs table with all columns — PARITY.md:876 — FULL
22. Proxy logs tab — PARITY.md:877 — MISSING
23. Console output view — PARITY.md:877 — MISSING
24. Visual request timeline (request shown step by step) — PARITY.md:877 — PARTIAL (see note)
25. Activity feed — PARITY.md:877 — MISSING
26. Conversations view (multi-turn chats) — PARITY.md:877 — MISSING
27. Compression log tab — PARITY.md:877 — MISSING
28. Audit log (who did what, when) — PARITY.md:877 — MISSING
29. Routing strategy dropdown, wired to the real global strategy — PARITY.md:878 — FULL
30. Switches for automatic failover, prompt-cache pinning, cost-aware routing, health checks, request logging — PARITY.md:879 — MISSING
31. Listen address + port settings — PARITY.md:880 — FULL
32. Authorisation / route inventory page (which routes need which key) — PARITY.md:881 — FULL
33. Require login / password change — PARITY.md:882 — DIVERGENCE (we use gateway keys instead of passwords; not a build item)
34. Benchmark page — PARITY.md:883 — EXTRA (ours-only, already built)
35. `priority` strategy: use providers in the order you listed them — PARITY.md:884 — MISSING
36. `round-robin` strategy: take turns across providers — PARITY.md:885 — MISSING
37. `context-relay` strategy: hand off to another provider after a success — PARITY.md:886 — MISSING
38. `fill-first` strategy: keep the listed order until a target fails or runs out — PARITY.md:887 — MISSING
39. `p2c` strategy: pick the better of two random choices — PARITY.md:888 — MISSING
40. `random` strategy: pick a provider at random — PARITY.md:889 — MISSING
41. `least-used` strategy: send to the least-used target — PARITY.md:890 — MISSING
42. `cost-optimized` strategy: pure cheapest-first — PARITY.md:891 — PARTIAL
43. `reset-aware` strategy: weigh by when quota resets — PARITY.md:892 — MISSING
44. `reset-window` strategy: weigh by reset window — PARITY.md:893 — MISSING
45. `expiry-first` strategy: use the quota that expires first — PARITY.md:894 — MISSING
46. `headroom` strategy: rank by remaining quota headroom — PARITY.md:895 — MISSING
47. `quota-weighted` strategy: draw weighted by score and load — PARITY.md:896 — MISSING
48. `strict-random` strategy: shuffled deck, no repeats until the deck is done — PARITY.md:897 — MISSING
49. `auto` 16-factor strategy — PARITY.md:898 — PARTIAL
50. `lkgp` strategy: learn from the last good provider — PARITY.md:899 — MISSING
51. `context-optimized` strategy: biggest context window first — PARITY.md:900 — PARTIAL
52. `cache-optimized` strategy: order by prompt-cache affinity — PARITY.md:901 — MISSING
53. `fusion` strategy: ask several providers in parallel, a judge merges the answers — PARITY.md:902 — MISSING
54. `pipeline` strategy: multi-stage routing, one stage feeds the next — PARITY.md:903 — MISSING
55. `jev` strategy: an external judge scores answers, weak ones held back — PARITY.md:904 — MISSING
56. Zero-config auto-routing on/off toggle — PARITY.md:905 — PARTIAL
57. Sticky Limit: how many calls one account gets before round-robin switches — PARITY.md:906 — MISSING
58. Concurrency, queue timeout and queue depth settings for combos — PARITY.md:907 — PARTIAL
59. Track Metrics on/off toggle — PARITY.md:908 — PARTIAL
60. Max Retries / Retry Delay (ms) settings — PARITY.md:909 — PARTIAL
61. Max Nesting Depth setting — PARITY.md:910 — MISSING
62. Provider Overrides: per-provider max retries — PARITY.md:911 — MISSING
63. Failover Before Retry on/off toggle — PARITY.md:912 — PARTIAL
64. Max Set Retries / Set Retry Delay settings — PARITY.md:913 — MISSING
65. Combo Quota Only Fallback toggle — PARITY.md:914 — MISSING
66. Reasoning Token Buffer toggle — PARITY.md:915 — MISSING
67. Response Validation editor (rules for what a 200-OK body must look like) — PARITY.md:916 — MISSING
68. Auto-combo catalog variants: subscription, thrifty, offline, smart, lkgp, chaos, auto/&lt;cat&gt;:&lt;tier&gt;, best-*/pro-* — PARITY.md:917 — PARTIAL
69. LKGP toggle + Clear LKGP Cache button — PARITY.md:918 — MISSING
70. System-block Transform Pipeline: list of operations, up to 50, JSON import/export — PARITY.md:919 — MISSING
71. CLI Fingerprint Matching per provider — PARITY.md:920 — MISSING
72. Client Cache Control: Always / Never / Auto preserve — PARITY.md:921 — MISSING
73. Echo requested model name setting — PARITY.md:922 — MISSING
74. Web search routing model picker — PARITY.md:923 — MISSING
75. Adaptive Volume Routing toggle — PARITY.md:924 — MISSING
76. Antigravity Signature Cache Mode — PARITY.md:925 — MISSING
77. 16-factor scoring + 6 mode packs — PARITY.md:926 — MISSING
78. Scoring inspector: see each factor's score, recompute read-only — PARITY.md:927 — PARTIAL
79. Account fallback strategies (9 choices) — PARITY.md:928 — MISSING
80. Hedging control: delay and on/off — PARITY.md:929 — PARTIAL
81. Max attempts clamp [1,200] setting — PARITY.md:930 — PARTIAL
82. Candidate-pool endpoint: see the candidate targets for an auto-combo — PARITY.md:931 — MISSING
83. Combo Studio live cascade view — PARITY.md:932 — MISSING
84. Per-key model restriction: choose which models a key may use ("All models" picker) — PARITY.md:933 — PARTIAL
85. Exclusive Lease badge on a key — PARITY.md:934 — MISSING
86. JSON-stream default badge on a key — PARITY.md:935 — MISSING
87. Device tracking per key: how many distinct IPs / browsers, last 30 min — PARITY.md:936 — MISSING
88. Requests + Last used shown on the key card — PARITY.md:937 — PARTIAL
89. Access schedule per key (when the key works) — PARITY.md:938 — MISSING
90. Show / Hide key button per key — PARITY.md:939 — PARTIAL
91. Per-key delete, wired (not just a toast) — PARITY.md:940 — PARTIAL
92. Save a key as a template — PARITY.md:941 — MISSING
93. Edit key permissions: expiry, RPM, TPM, guardrails allowlist; model/endpoint/connection allow-lists — PARITY.md:942 — PARTIAL
94. Per-key routing page — PARITY.md:943 — MISSING
95. Per-key access page: allowed models, connections, endpoints, combos — PARITY.md:944 — MISSING
96. Key import (bring keys in from elsewhere) — PARITY.md:945 — MISSING
97. Key pools / fair-share quotas — PARITY.md:946 — MISSING
98. IP allowlist / blocklist per key — PARITY.md:947 — MISSING
99. Management Access Tokens: read/write/admin, expiry, revoke, shown once — PARITY.md:948 — MISSING
100. Warn at %: budget warning threshold per key — PARITY.md:949 — MISSING
101. Reset Interval / Reset Time (UTC) / Next reset display — PARITY.md:950 — MISSING
102. Budget templates: $/month, $/day, apply to selected keys — PARITY.md:951 — MISSING
103. Spend projection: linear trend, projected end-of-month, "above limit/month" warning — PARITY.md:952 — MISSING
104. 30-day cost breakdown by provider, per key — PARITY.md:953 — PARTIAL
105. Budget table: Key / Today / Month / Daily limit / Monthly limit / Used% / Status, with sorting — PARITY.md:954 — MISSING
106. Per-key token limits: per model/provider/global, strictest wins, 429 when hit — PARITY.md:955 — PARTIAL
107. Free-Tier Budget page — PARITY.md:956 — MISSING
108. Free Provider Rankings by model ELO — PARITY.md:957 — MISSING
109. Provider Quota page: quota-limit UI — PARITY.md:958 — PARTIAL
110. Token usage totals: in/out tokens and in/out ratio — PARITY.md:959 — PARTIAL
111. Routing efficiency: fallback requests/rate, model coverage — PARITY.md:960 — PARTIAL
112. Cost Trend / Provider Share / Top Providers / Top Models charts — PARITY.md:961 — PARTIAL
113. Cost by API Key table — PARITY.md:962 — MISSING
114. Cost by Account table — PARITY.md:963 — MISSING
115. Weekly pattern / 365-day heatmap / period comparison — PARITY.md:964 — MISSING
116. Pricing rate table: input/output/cached/reasoning/cache-write $/1M, with sync source per row — PARITY.md:965 — PARTIAL
117. Pricing sync: models.dev/LiteLLM, Sync Now, last/next sync, clear synced — PARITY.md:966 — MISSING
118. Utilization view: global/account split, quota snapshots, remaining capacity — PARITY.md:967 — MISSING
119. Cache analytics: hit rate, tokens saved, provider breakdown, 24h trend — PARITY.md:968 — PARTIAL
120. Compression analytics — PARITY.md:969 — MISSING
121. Search analytics — PARITY.md:970 — MISSING
122. Evals tab — PARITY.md:971 — MISSING
123. Provider Stats page: latency/performance per provider — PARITY.md:972 — PARTIAL
124. Telemetry p50/p95/p99 — PARITY.md:973 — PARTIAL
125. Time range filter on logs — PARITY.md:974 — PARTIAL
126. Log export in JSON (CSV already exists) — PARITY.md:975 — PARTIAL
127. Log export destinations: scheduled exports, high-water mark — PARITY.md:976 — MISSING
128. Resilience settings page: queue, cooldown, breaker, wait budgets — PARITY.md:977 — MISSING
129. CORS allowed origins setting — PARITY.md:978 — MISSING
130. Blocked providers from `/v1/models` setting — PARITY.md:979 — MISSING
131. IP filter section — PARITY.md:980 — MISSING
132. MFA (multi-factor login) — PARITY.md:981 — MISSING
133. PII masking card in Security settings — PARITY.md:982 — PARTIAL
134. Feature flags grid — PARITY.md:983 — MISSING
135. Appearance settings: Light/Dark/System, palettes, branding — PARITY.md:984 — PARTIAL
136. AI Settings: thinking budget, system prompt, etc. — PARITY.md:985 — MISSING
137. Modality Bridge settings — PARITY.md:986 — MISSING
138. Cache settings UI: enable, threshold, TTL, engine, embedding provider/model, test — PARITY.md:987 — PARTIAL
139. Sidebar customisation: presets, pinning, reorder — PARITY.md:988 — MISSING
140. Storage settings: DB stats, backups/restore, retention days, vacuum, export/import, reset usage — PARITY.md:989 — PARTIAL
141. Advanced settings: payload rules, body limit, model aliases, lockout card — PARITY.md:990 — MISSING

## Not counted in the 141 (prerequisite, separate row)

- Dashboard login/password/session itself: no dashboard password, session, or
  login of any kind today; protection is loopback-bind only — PARITY.md:653 —
  MISSING. BURNDOWN Tier 2 lists it as the blocker dashboard trust depends on.
  It is not in the Area 8b table, so it is not one of the 141 — but B8 cannot be
  used safely without it. Decide separately whether it belongs in Stage 3.

## Count note (why 141, not 114)

- The Area 8b table in PARITY.md has **133 data rows**: FULL 9, PARTIAL 36,
  MISSING 86, DIVERGENCE 1, EXTRA 1.
- Two rows are composites that bundle several features: PARITY.md:868 (Daily /
  Weekly / Monthly $ limit = 3 features in 1 row) and PARITY.md:877 (7 log views
  in 1 row). Splitting them per the source comparison report
  (`omniroute-full-comparison-2026-11-18.md` §Area 8b, which lists Daily/Weekly/
  Monthly as 3 rows and the 7 log views as 7 rows) adds 8 lines: 133 − 2 + 10 =
  **141 features**.
- Split verdicts: **FULL 9, PARTIAL 37, MISSING 93, DIVERGENCE 1, EXTRA 1.**
  Comparable = 141 − 1 DIVERGENCE − 1 EXTRA = **139, exactly the Area 8b header's
  139**. MISSING 93 and FULL 9 also match the header exactly.
- The "114" from the prompt: before EXPAND-8B, 114 open rows (81 MISSING + 33
  PARTIAL) existed only in the comparison report. EXPAND-8B itemized 107 of them;
  the other 7 stayed bundled in the 2 composite rows. Splitting the composites
  finds 8 bundled features, not 7 — the 1 difference is the Timeline view
  (line 24 above): the source report counts it PARTIAL, but the file's composite
  row counts it MISSING. That is why the header says PARTIAL 36 while the split
  count is 37. It is the same single "1 unaccounted" row BURNDOWN.md already
  documents for Area 8b; no feature was invented or dropped to close it.
- Open features to decide on: **130** (93 MISSING + 37 PARTIAL). Already done:
  9 FULL. Not build items: 1 DIVERGENCE (password login — we use gateway keys),
  1 EXTRA (Benchmark page — ours-only, already built).
