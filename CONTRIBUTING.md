# Contributing to epitrop

Thank you for contributing! This repository adheres strictly to the
**High-Integrity Systems Standard (HISS)** and modernized **NASA JPL
Power-of-10** rules.

## Core Directives & Verification

All changes must pass local verification before submitting:

```bash
make verify-all
```

### Modernized NASA JPL Power-of-10 Rules

1. **Simple Control Flow (HISS-01)**: Recursion is strictly banned; call graph
   must be an acyclic DAG; zero `goto`.
2. **Bounded Loops (HISS-02)**: All loops must have a statically verifiable
   scalar upper bound. Network and disk I/O require `context.Context` timeout.
3. **Deterministic Memory (HISS-03)**: Zero dynamic heap allocations
   (`malloc` / `free`) in hot simulation or rendering loops.
4. **Function Length Cap (HISS-04)**: No function may exceed
   **60 lines of code** ($\le 60$ LOC).
5. **Assertion Density (HISS-15)**: Functions must assert preconditions, state
   invariants, and postconditions.
6. **Data Scope**: Variables must be declared at the smallest possible scope.
7. **Checked Errors (HISS-07)**: Check return values of all non-void functions;
   zero `.unwrap()` or unchecked errors.
8. **Static Execution (HISS-08)**: Dynamic code evaluation (`eval` / `exec`)
   and banned unsafe libc calls (`gets` / `strcpy` / `sprintf`) are prohibited.
9. **Pointer Safety (HISS-09)**: Pointer arithmetic must be bounded; all
   `unsafe` blocks require `// SAFETY:` justifications.
10. **Zero-Warning Hygiene (HISS-10)**: Zero compiler, linter, or formatting
    warnings tolerated across all builds.

### 3D Testing Discipline (HISS-15)

Every public function requires:

- **Positive tests**: Expected valid operational inputs.
- **Negative tests**: Invalid inputs, expected error returns.
- **Boundary tests**: Zero, one, max limits, off-by-one bounds.

### Commit Messages

We enforce Conventional Commits:

- `feat:` New features
- `fix:` Bug fixes
- `chore:` Maintenance and governance
- `feat!:` / `fix!:` Breaking API changes (must include `Migration:` footer)

A commit message or a pull request text names no model and no coding tool as an author of the change: no co-author trailer for one, no "generated with" line. `tools/commitmsg` refuses both shapes. CI runs it on the commits and the text of every pull request and on every push to `main`; `make commit-messages` runs it on the commits your branch adds to `origin/main`, and on a branch that adds no commit to `origin/main` it prints a skip line and passes.
