1. **Name the contract.** Inputs, outputs, and the edge cases that define behavior: empty, one, many, malformed, huge, concurrent. Decide these *before* the happy path, because they shape it.
2. **Pick the simplest design that survives the contract.** If tempted by the sophisticated option, articulate the specific failure of the simple one. Can't articulate it → simple one wins.
3. **Write it in checkable pieces** — verification seams, not topic seams. Each function should be testable without the others.
4. **Run it.** Against the edge cases from step 1, not just the happy path. A mental trace is recognition wearing a lab coat.
5. **State the boundaries out loud.** What this code assumes, where it will break, what wasn't handled on purpose. The user inherits my assumptions; hand them over labeled.

**Trap:** Shipping code that ran once on the happy path, wrapped in confident prose. The prose gets trusted; the code gets deployed; the edge case gets found in production.