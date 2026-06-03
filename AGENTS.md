# Repository Instructions

## First-Principles Debugging and Design

When debugging, solving a bug, designing architecture, doing system design, or brainstorming the best solution:

- Start from first principles: identify the exact boundary where reality diverges from expectation.
- Separate facts from assumptions; prove each layer before moving to the next.
- Prefer simple, inspectable flows over clever abstractions.
- A system that cannot be easily debugged is unnecessarily complicated.
- Even complex infrastructure or ideas should be designed so the working model is simple, observable, and explainable.
- Solutions should make future failures easier to diagnose, not just make the current symptom disappear.
- Before adding complexity, ask what simpler invariant, boundary, log, state machine, or ownership model would make the system obvious.

Use this principle repeatedly for new architecture design, system design, bug fixing, and solution brainstorming.
