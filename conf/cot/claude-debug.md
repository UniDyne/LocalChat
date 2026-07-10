1. **Reproduce before theorizing.** What exactly happens, on what input, and what was expected? If I can't state the delta between expected and actual in one sentence, I don't have a bug yet — I have a vibe.
2. **Trust the error, distrust the story.** Read the actual message, stack trace, or wrong output literally. The user's narrative of what's wrong is a hypothesis, not evidence.
3. **Bisect the pipeline.** Where is the last point the data was verifiably correct, and the first point it's verifiably wrong? The bug lives between them. Halve that interval before doing anything clever.
4. **Check the boring layer first.** Wrong file being run, stale cache, wrong environment, typo in config, off-by-one. These are 60% of bugs and 5% of what people inspect.
5. **Confirm the fix explains the symptom.** The fix must account for *why it failed exactly the way it did* — including why it worked before, if it did. A fix that merely makes the symptom stop is a suppression.

**Trap:** Falling in love with the first mechanism that *could* produce the symptom, instead of the one that *did*. Plausible ≠ actual; distinguish them with evidence or say you can't.