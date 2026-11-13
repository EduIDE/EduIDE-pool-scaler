# EduIDE Pool-Scaler

Decides, per AppDefinition, how many **free** prewarmed IDE pods an EduIDE
installation keeps ready. It writes one number, `spec.targetIdle` on the
AppDefinition. The operator in [EduIDE-Cloud](https://github.com/EduIDE/EduIDE-Cloud)
does the rest: it keeps that many unused pods, refills after every claim and
removes surplus ones.

```
 providers                     strategies                  guardrails                   actor
 kubernetes  ─┐                ┌ static          ┐
 prometheus  ─┤                ├ reactive        ┤   floor/ceiling, reactive    patch spec.targetIdle
 artemis     ─┼─▶ snapshot ─▶  ├ predictive      ┼─▶ lower bound, step limits, ─▶ (mode: active)
 calendar    ─┘                └ decision-model  ┘   hysteresis, cooldown,
                                 (one active,         fallback chain, override,
                                  rest shadow)        budget
```

- The operator is the mechanism, the scaler is the policy.
- Without the scaler `targetIdle` stays where it is. Without `targetIdle` the
  operator behaves as before (`minInstances` is the pool size).
- Fail-static: when the scaler cannot see the pools, it changes nothing.

The design and its reasoning are in the plan
`plans/2026-11-13-prewarm-pool-autoscaler.md` of the EduIDE workspace.

## Install

The `eduide` Helm chart ships the scaler as an optional component:

```yaml
poolScaler:
  enabled: true
  mode: shadow          # start here, compare, then switch to active
  config:
    timezone: Europe/Berlin
    strategies: [reactive, predictive]
    prometheus:
      url: http://prometheus-operated.cattle-monitoring-system:9090
  monitoring:
    enabled: true       # Grafana board, PodMonitor and alerts
```

One replica, `Recreate`, no leader election. RBAC: `get/list/watch/patch`
on AppDefinitions and read-only `pods`, `resourcequotas` in its namespace,
read-only `nodes` and `pods` cluster-wide (for the node budget).

## Strategies

All strategies run every round. One is active, the others run in shadow and
export what they would have done (`pool_scaler_desired_idle{active="false"}`).

| Strategy | Recommends |
|---|---|
| `static` | A fixed value per AppDefinition (`appLimits.<app>.static`, else the floor). The baseline of the switchback experiment. Not lifted to reactive. |
| `reactive` | claim rate of the last `reactive.window` × p95 pool pod ready time × (1 + `safetyFactor`). Base, last fallback and lower bound for every other strategy. |
| `predictive` | Weekly profile of claims per weekday and quarter hour (EWMA over `trainWeeks`), plus an uplift around known events (participants × factor, spread over the event window). Horizon = p95 ready time + `horizonBuffer`. The forecast is a Poisson distribution over `levels`; `targetIdle` is the smallest level with P(demand ≤ level) ≥ `quantile`. Trained from Prometheus at start and every `retrainInterval`, no database. Event factors are re-estimated from past events once `minEventsForFactor` of a kind exist. |
| `decision-model` | Asks an externally hosted decision model (Open-Jev style) for a probability per demand level, given the state as text (aggregated numbers only, the predictive forecast, upcoming events). Same quantile rule. Called every `decisionModel.interval`; the answer is reused in between and dropped after two intervals. |

### Decision model contract

`DecisionClient` is the seam. `HTTPDecisionClient` posts

```json
{"question": "<state as text>", "choices": ["0", "1", "2", "...", "160+"]}
```

with `Authorization: Bearer $DECISION_MODEL_TOKEN` over https only, and
expects `{"probabilities": [...]}`, one per choice. Adapt `Decide` once the
hosted model's schema is fixed. `FakeDecisionClient` answers from a function.

An answer counts as failed on timeout, error, an invalid distribution or a
target more than `plausibilityFactor` off predictive. `breakerFailures`
failures in a row open a circuit breaker for `breakerPause`, then one test call
goes through.

## Guardrails

Applied after the strategy, in this order:

1. **Fallback chain**: active strategy → `predictive` → `reactive` → last
   valid value → floor. Every step down the chain counts in
   `pool_scaler_strategy_fallbacks_total{strategy, reason}`.
2. **Never below reactive** (except for `static`).
3. **floor / ceiling** per AppDefinition (`defaults`, `appLimits`), and never
   above `maxInstances`.
4. **Step limits**: at most `maxStepUp` up and `maxStepDown` down per round.
   **Hysteresis**: no scale-down by less than `hysteresis`. **Cooldown**: no
   scale-down within `scaleDownCooldown` of an increase. Floor and ceiling win
   over step limits.
5. **Budget** (below).

The guard keeps its own "current value", so shadow mode behaves exactly like
active mode would.

## Budget

Free budget = minimum of

1. the namespace ResourceQuota rest (`requests.cpu`, `requests.memory`),
2. unrequested allocatable on the nodes matching `budget.nodeSelector`,
3. `usageHeadroomFraction` × free memory by actual usage (`budget.usageQuery`,
   off when empty),

plus what the current idle pods already hold. Every floor is granted first;
the rest is shared in proportion to each deficit (`desired - floor`), rounded
down, with one scale factor on the scarcest resource. A pool pod's size is the
AppDefinition's requests plus its sidecars plus `budget.podOverhead` (the
oauth2-proxy). There is no reserve: real sessions push idle pods out
(operator, phase C). Cuts count in `pool_scaler_budget_clamped_total`.

## Manual control

| Annotation on the AppDefinition | Effect |
|---|---|
| `pool-scaler.eduide/override-target-idle: "40"` with `pool-scaler.eduide/override-until: "2026-12-10T12:00:00+01:00"` | Fixed value until the end time. Only budget and `maxInstances` still apply. An override without an end time is ignored. |
| `pool-scaler.eduide/paused: "true"` | The scaler leaves this AppDefinition alone. |

```bash
kubectl annotate appdefinition java-17 \
  pool-scaler.eduide/override-target-idle=40 \
  pool-scaler.eduide/override-until=2026-12-10T12:00:00+01:00
```

`mode: shadow` stops all patching globally.

## Switchback experiment

With `switchback.enabled`, the active strategy changes per day (in
`timezone`): every weekday runs each of `switchback.strategies` once per block
of that many weeks, in an order drawn from `seed`. Days with an exam always run
`examDayStrategy`. `switchback.schedule` pins a strategy to a date. Every
decision metric carries `active_strategy`.

## Events

- **Artemis**: `GET <artemis.url>/api/programming/admin/online-ide/upcoming?days=N`
  with `Authorization: Bearer $ARTEMIS_TOKEN`. An exam becomes one `exam`
  event; an exercise a `release` (start date, else release date) and a `due`
  event. Fetched every `artemis.interval`; a failed fetch keeps the last
  events.
- **Calendar**: a YAML file (`calendar.file`), re-read every round. See
  [examples/calendar.yaml](examples/calendar.yaml).

## Metrics

Served on `:9090/metrics`.

| Metric | Labels |
|---|---|
| `pool_scaler_desired_idle` | `appdef`, `strategy`, `active`, `active_strategy` |
| `pool_scaler_target_idle` | `appdef`, `active_strategy` |
| `pool_scaler_forecast_starts` | `appdef`, `strategy` |
| `pool_scaler_forecast_demand_probability` | `appdef`, `strategy`, `level` (P(demand ≤ level)) |
| `pool_scaler_budget` | `resource` (cpu cores, memory bytes), `limit_source` (`quota`, `nodes`, `usage`) |
| `pool_scaler_budget_clamped_total` | `appdef` |
| `pool_scaler_provider_up`, `pool_scaler_provider_errors_total` | `provider` |
| `pool_scaler_strategy_errors_total` | `strategy` |
| `pool_scaler_strategy_fallbacks_total` | `strategy`, `reason`, `active_strategy` |
| `pool_scaler_override_active`, `pool_scaler_paused` | `appdef` |
| `pool_scaler_last_decision_timestamp_seconds` | |
| `pool_scaler_patches_total` | `appdef`, `result` |
| `pool_scaler_decisions_total` | `appdef`, `source` (a strategy, `last-valid`, `floor`, `override`), `active_strategy` |
| `pool_scaler_upcoming_events` | `appdef` |
| `pool_scaler_active_strategy`, `pool_scaler_mode` | `strategy`, `mode` |
| `pool_scaler_decision_model_breaker_open` | |

Every round logs one JSON line per AppDefinition with the decided value and
why.

## Configuration

[examples/config.yaml](examples/config.yaml) lists every field with its
default. Unknown fields are rejected. Secrets come from the environment:
`ARTEMIS_TOKEN`, `DECISION_MODEL_TOKEN`.

## Inputs it reads

From the operator (EduIDE-Cloud):

- AppDefinition `spec.minInstances`, `maxInstances`, `targetIdle`, requests,
  sidecars, annotations (`theia.cloud/v1beta11`).
- Pool pods labelled `pool-state=idle|claimed` and `app=<appdefinition>-<n>`;
  Ready condition splits idle into ready and starting.
- `eduide_session_claims_total` (claim rate, training history) and
  `eduide_pool_instance_ready_seconds` (p95 ready time), label `app_definition`.

## Backtesting

```bash
go run ./cmd/pool-scaler backtest \
  --prometheus http://127.0.0.1:9090 --namespace eduide-tum-production \
  --from 2026-11-01T00:00:00Z --to 2026-11-08T00:00:00Z \
  --strategies static,reactive,predictive --config examples/config.yaml
```

Replays the recorded claims step by step against each strategy with the same
guardrails (no budget) and a simulated operator that refills to `targetIdle`
with pods that need `--ready-seconds` to become ready. Reports per strategy
and AppDefinition: claims served by a ready pod, by a starting pod, cold, the
warm-ready ratio and idle pod hours. `predictive` trains once on the
`trainWeeks` before `--from`. `decision-model` calls the configured model.

## Development

```bash
go test ./...
golangci-lint run ./...
go run ./cmd/pool-scaler --config examples/config.yaml   # uses your kubeconfig
```

Keep `mode: shadow` when running against a real cluster from a laptop.

## License

[Eclipse Public License 2.0](LICENSE), like EduIDE-Cloud.
