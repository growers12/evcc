# Fork changes: better SoC handling for cars that go quiet while charging

This is a fork of [evcc-io/evcc](https://github.com/evcc-io/evcc), based on the **0.313.1** release
tag, with two additions. Everything else is upstream, unmodified.

If you found this fork looking for one of these problems, the notes below should tell you quickly
whether it is worth your time.

---

## The problem both changes address

Some vehicles do not report their state of charge **while they are plugged in**. They sleep, they sit
in a garage without mobile reception, or their cloud API only refreshes when the car is awake. For
the whole charging session the vehicle API keeps returning the same value it last sent before you
arrived.

evcc's SoC estimator (`soc: {estimate: true}`) exists for exactly these vehicles: it extrapolates the
current SoC from the energy delivered at the charger. That works well — until something interrupts
it.

---

## 1. The estimate survives a restart

**Symptom.** evcc restarts mid-session — an update, a reboot, a power cut. Afterwards the displayed
SoC has fallen back to the vehicle's stale reading, and the estimator's accumulated correction is
gone. If you have `minSoc` configured above that stale value, evcc immediately starts charging from
the grid towards a level the car reached hours ago.

**What this fork does.** It persists the estimator's *anchor* — the last soc the vehicle itself
reported, and the energy delivered since — per vehicle under the existing `vehicle.<name>.<key>`
settings scheme, and restores it into the estimator after a restart.

The anchor rather than the estimate, because `Soc()` recomputes the estimate from the anchor on every
poll; a restored estimate would be overwritten by the next call. A useful side effect is that the
restored correction expires by itself: as soon as the vehicle reports a genuinely different value,
the estimator's own rebase branch resets the anchor. **A real reading always wins.**

This was offered upstream as [evcc-io/evcc#32485](https://github.com/evcc-io/evcc/pull/32485) and
declined — the maintainer considered it too much code for a situation he sees mainly in developer
setups, and noted that a restored SoC is only a heuristic. Both points are fair; the need here is
simply real enough to keep maintaining.

## 2. The gradient is learned across the session boundary

**Symptom.** `energyPerSocStep` — the Wh needed per SoC percentage point — never improves. Upstream
learns it by watching the vehicle's reported SoC rise *during* a charge. A vehicle that reports
nothing while plugged in never triggers that, so the value stays at its static default
(`capacity / ChargeEfficiency / 100`) forever.

That default is optimistic in two ways at once: people often enter gross rather than usable capacity,
and the 0.85 efficiency factor is pessimistic for a three-phase 11 kW charge, which reaches 90 %+ in
practice. Measured values here landed roughly 15 % below the default.

**What this fork does.** It learns retroactively, at the moment the car regains reception after
leaving:

```
energyPerSocStep = energySinceAnchor / (freshSoc − anchorSoc)
```

Three guards keep it honest: a rise of more than 10 percentage points, a result within 0.5×–2× of the
nominal value, and an exponential moving average (factor 0.3) rather than overwriting. A `samples`
counter tells you whether anything was ever learned.

**Requires `soc: {poll: {mode: always}}`.** With `connected`, evcc stops polling the vehicle once it
is unplugged — which is precisely when the car finally reports a fresh value.

## Reading and correcting the estimate

| Endpoint | Effect |
|---|---|
| `GET /api/loadpoints/{lp}/soc` | full estimator and record state, including `learned` and `samples` |
| `POST /api/loadpoints/{lp}/soc/{percent}` | set the estimate |
| `POST /api/loadpoints/{lp}/soc/energy/{kWh}` | shift the anchor by an amount of energy |
| `DELETE /api/loadpoints/{lp}/soc` | drop the override and follow the vehicle again |
| `GET /api/vehicles/{name}/socestimate` | the persisted record, also for a vehicle that is not plugged in |

Setting a value **below** what the vehicle reports is rejected with `400`. It would otherwise answer
`200` and silently revert within one poll, because the estimator rebases onto the lower source value.
The vehicle's own reading is the floor.

---

## 3. Splitting a charging session without unplugging

Unrelated to the SoC work, but part of the same fork.

`POST /api/loadpoints/{lp}/session/split[/{vehicle}]` ends the running session and starts a new one
**without a disconnect**. This is for setups where a different car ends up on the same cable and evcc
never sees a status change, so all the energy would otherwise be booked to one vehicle.

| Call | Meaning |
|---|---|
| `…/session/split` | cut, keep the vehicle |
| `…/session/split/{name}` | cut and assign `{name}` |
| `…/session/split/none` | cut and detach (guest vehicle) |

`none` is resolved against the vehicle list first, so a vehicle actually named that still wins. With
no energy charged yet the endpoint answers `409`.

**One caveat worth knowing:** the lossless energy rebase relies on `lp.chargeRater` being evcc's own
`wrapper.ChargeRater`. If your charger implements `api.ChargeRater` itself, `ResetCharge()` becomes a
no-op and each split counts the first leg's energy again in the second — silently, with plausible
numbers. The code logs a WARN when the type assertion fails; if you see that line, do not trust the
session figures.

Known limitation: after a vehicle swap the new session's `soc_start` stays empty, because
`setActiveVehicle()` resets `vehicleSoc` via `unpublishVehicle()`. The kWh are correct.

---

## Using this

The branch `master` is the 0.313.1 tag plus the above. It deliberately does **not** track upstream
master — pinning to a tested release is the point, so GitHub will show this fork as "behind".

Build it like upstream evcc (`make` after `make install-ui && make install`), or clone this branch in
your own Docker build.

On a version bump, rebase `master` onto the new tag. Two things `git apply` cannot catch and that a
rebase can quietly reopen:

- **`restoreSocEstimate` has to run *after* `energyMetrics.Reset()`.** `evVehicleConnectHandler` calls
  `vehicleDefaultOrDetect()` — and with it `setActiveVehicle` — before `createSession()`, and
  `createSession()` is what zeroes the session counter the anchor is measured against. Anchoring
  before that reset looks correct at process start (the counter really is zero there) but collapses
  the estimate onto the stale reading on every *subsequent* plug-in, and overwrites the stored record
  with zero. The restore therefore lives at the end of `createSession()`.
- **`SetSoc` must not move `prevSoc`, `Restore` must set it.** These sound contradictory and are not:
  `SetSoc` works on an already-anchored estimator, `Restore` on a fresh one whose `prevSoc` is 0 and
  whose first poll would otherwise take the rebase branch and discard everything restored.

There is deliberately **no odometer guard** on the learner. It was tried and removed: a vehicle that
writes its odometer at ignition-off — in the garage, without reception — reports a distance that
describes the drive *before* the anchor, which cannot corrupt the measurement. The guard rejected
every real learning opportunity while protecting against nothing.

Unit tests for both features live in the branch; `go test ./core/... ./server/`.

---

Issues and PRs against this fork are welcome, but note it is maintained for one specific setup and
does not track upstream releases closely. For anything not in the list above, please use
[upstream evcc](https://github.com/evcc-io/evcc).
