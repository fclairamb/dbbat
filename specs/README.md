Specs should be initially written in github issues. And once we are ready
to implement them, they should be written to the specs directory, re-edited and implemented.

All files specs should contain a link to their initial github issue. 
Something like that: [#4](https://github.com/fclairamb/dbbat/issues/4)

## Layout and naming

```
specs/todos/YYYY-MM-DD-NN-short-kebab-name.md   # queued, in execution order
specs/done/YYYY/MM/YYYY-MM-DD-NN-short-kebab-name.md   # implemented
```

`NN` is a two-digit **execution order** within the date. Sorting the directory
by name therefore sorts the backlog by "what to do first": oldest date first,
and within a date, lowest number first. `01` is the next thing to pick up.

The number is a priority, not a timestamp — order by what must happen first:

1. **Prerequisites and enablers** — a one-line fix that makes a test suite
   runnable belongs ahead of the work that needs to run it.
2. **Blind test infrastructure** — a suite that dies at setup verifies nothing,
   and every spec after it is being merged on weaker evidence than it looks.
3. **Correctness and security bugs in shipped code.**
4. **Completeness of shipped features** — asymmetries, missing coverage.
5. **New capability and deferred decisions.**
6. **Externally gated work** — e.g. something that must wait for a release.

When filing a new todo, append it after the last `NN` of that date unless it
genuinely blocks something already queued, in which case renumber the ones it
blocks. When a spec moves to `specs/done/YYYY/MM/`, keep its filename — the
number stays part of the historical record.

A spec that other files reference by path (a code comment, a docs page) should
be grepped for before renaming: `grep -rn specs/todos/ --include='*.go' --include='*.md'`.
