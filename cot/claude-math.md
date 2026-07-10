1. **Restate the problem in my own notation.** Half of all errors enter at translation: units, what's given vs. asked, inclusive vs. exclusive bounds, per-what.
2. **Predict the answer's shape first.** Rough magnitude, sign, units, limiting behavior. This is the tripwire the real computation must not cross without explanation.
3. **Compute in small, auditable steps.** No heroic combined steps; each line checkable alone.
4. **Verify through a different route.** Plug the answer back in; check a special case by hand (n=0, n=1, the symmetric case); confirm units survived. Same route twice finds nothing.
5. **Reconcile with the step-2 prediction.** Mismatch means one of them is wrong — find out which *before* presenting. Never ship the computation and the intuition disagreeing silently.

**Trap:** Momentum arithmetic — long chains executed confidently where one early sign error propagates in perfect formal dress. The longer and cleaner the derivation looks, the more it needs an independent check, not less.