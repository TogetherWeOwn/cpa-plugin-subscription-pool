# synthetic module provenance

Written from scratch for this repository. Inputs:

- synthetic.new public documentation: `https://synthetic.new/rate-limits` (weekly credit limit only; 2% regenerates every 202 minutes; no five-hour or monthly limit), `https://synthetic.new/pricing` (Big plan: infinite mode, fast mode, extra usage at API pricing), `https://dev.synthetic.new/docs/api/models` (infinite aliases, `infinite:small:vision`, streaming required, does not count against limits), `https://dev.synthetic.new/docs/synthetic/quotas` (`GET /v2/quotas`, free to call), and the public model catalogue `https://api.synthetic.new/openai/v1/models`.
- Synthetic's MIT-licensed client `synthetic-lab/octofriend` was read for the current quota shapes only (`planWeeklyLimit {percentRemaining, nextRegenAt, nextRegenPercent}`, legacy `weeklyTokenLimit` and `rollingFiveHourLimit`), the `x-synthetic-quotas` response header, and its 402/429 classification. No code was copied.
- CLIProxyAPI v8.0.38 source for the openai-compatibility auth-ID derivation, scheduler candidate attributes, priority tiers and `scheduler_across_priorities`. The golden auth IDs in `module_test.go` were produced by a running v8.0.38 instance.

Not documented by synthetic.new and therefore not assumed: a concurrency limit (`max-concurrent` defaults to no cap), the HTTP status and body at the weekly limit (both 429 and 402 are handled), infinite-mode queueing behavior, and whether `/v2/quotas` is the same shape for every plan.
