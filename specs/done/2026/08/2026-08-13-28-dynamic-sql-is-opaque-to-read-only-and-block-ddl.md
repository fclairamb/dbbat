# Dynamic SQL is opaque to `read_only` and `block_ddl` on every protocol that has it

**Filed against the owner's standing "no new specs" directive**, deliberately:
this is a live grant escape reachable today on `main`, on three protocols, and
it is the residual left by `2026-08-13-18` rather than something that spec
introduced. It needs to be a conscious release decision, not a surprise.

## Goal

Decide how far the grant controls should reach into a statement whose real
payload is a string literal, and close the statically decidable cases
consistently across MySQL, Oracle and SQL Server.

## Why

`2026-08-13-18` closed the `USE <db>` escape and, in doing so, exposed the
general shape of it. dbbat's two enforcement mechanisms behave differently:

- **Blocked-pattern lists** (`oracleBlockedPatterns`, `mysqlBlockedPatterns`)
  are *substring* matches, so they reach inside a literal by accident and
  usually still fire.
- **`read_only` / `block_ddl` classification** (`IsWriteQuery`, `IsDDLQuery`)
  is *prefix* based, so a statement whose first keyword is `PREPARE`, `BEGIN` or
  `EXEC` is classified on that wrapper and never on what actually runs.

Measured on the current tree, all under a grant carrying **both** `read_only`
and `block_ddl`:

| protocol | statement | result |
|---|---|---|
| MySQL | `DELETE FROM t` | refused (correct) |
| MySQL | `PREPARE s FROM 'DELETE FROM t'` | **allowed** |
| Oracle | `BEGIN EXECUTE IMMEDIATE 'DELETE FROM t'; END;` | **allowed** |
| SQL Server | `EXEC('DELETE ' + 'FROM t')` | **allowed** (literal concatenation) |
| Oracle | `BEGIN EXECUTE IMMEDIATE 'ALTER SESSION SET CONTAINER=PDB2'; END;` | blocked — but only because the blocked-pattern list is a substring match |

The last row is the tell: where dbbat happens to be protected, it is by
accident of matching style, not by design.

`2026-08-13-18` closed the SQL Server single-literal forms (`EXEC('…')`,
`EXECUTE('…')`, `sp_executesql N'…'` as batch text) and the MySQL
`PREPARE … FROM '<literal>'` **database-switch** case. It did **not** make
`read_only`/`block_ddl` reach inside any of them, and it did not touch Oracle.

Why it matters beyond the controls themselves: `queries` carries no database
column and, for these statements, no record of what actually executed. A
`PREPARE`/`EXECUTE` pair is logged as two statements neither of which is the
write that ran.

## Implementation

- Extract the payload of the statically decidable forms and run the **same**
  `ValidateQuery` classification over it that the outer statement gets, at
  depth 1, failing closed on nesting. `internal/proxy/shared/dynamicsql.go`
  already does exactly this for SQL Server (`MSSQLDynamicSQL`) — generalise it
  rather than adding a third implementation.
  - MySQL: `PREPARE <name> FROM '<literal>'` (single- and double-quoted, `''`
    doubling). The `EXECUTE <name>` half carries no text and cannot be checked
    on its own — the decision has to be recorded at `PREPARE` time, which means
    the session needs to remember what a prepared name resolves to, or refuse
    the `EXECUTE` of a name whose `PREPARE` was not checkable.
  - Oracle: `EXECUTE IMMEDIATE '<literal>'`, including inside a
    `BEGIN … END;` block, and `DBMS_SQL.PARSE`.
  - SQL Server: all-literal concatenation (`EXEC('DELETE ' + 'FROM t')`) —
    fold adjacent literal `+` operands before extracting.
- Decide the policy for the **undecidable** forms — `EXEC(@sql)`,
  `PREPARE … FROM @var`, `EXECUTE IMMEDIATE v_stmt`. Options, in increasing
  strictness: leave them (today's behaviour, now documented); refuse them under
  `read_only`/`block_ddl` only; refuse them whenever any control is set. Refusing
  is defensible precisely because a control-bearing grant is the case where
  "dbbat cannot tell what this runs" should not resolve to "allow" — but it
  will break legitimate application traffic that builds SQL at runtime, so it
  is a product decision, not an implementation detail.
- `EXEC(…) AT <linked_server>` routes the statement to another server entirely
  and is unenforced; scope it explicitly rather than leaving it implied.
- **The `sp_prepare` family sent as batch text.** `2026-08-13-18` shared the
  statement-parameter *name* list between the RPC and batch-text paths
  (`shared.IsMSSQLStatementParamName`), which closed `sp_executesql`. It did not
  do the same for `sp_prepare` / `sp_prepexec` / `sp_cursorprepare` /
  `sp_cursorprepexec`, which carry their statement at a **positional index**
  (`statementParamIndex` in `internal/proxy/mssql/rpc.go`) rather than by name.
  The RPC path enforces them; the batch-text scanner does not implement
  positional indexing at all, so this is unchecked today:

      EXEC sp_prepare @handle OUT, NULL, N'DROP TABLE Foo', 1   → not extracted, allowed

  Measured on the current tree. It is narrow — drivers send these as RPC, where
  they are enforced — but "no legitimate client spells it this way" is not a
  defence against someone who is choosing how to spell it. Fold the positional
  index into the same shared table as the name list so the two paths stay in
  step, which is the property `2026-08-13-18` established and this would extend.
- Tests: extend `internal/proxy/shared/dynamicsql_test.go` and each proxy's
  own suite. Pin the table above, and pin that a benign
  `EXEC('SELECT 1')` / `PREPARE s FROM 'SELECT 1'` stays allowed — this must
  not become a blanket refusal of dynamic SQL.
- Docs: `docs/mysql.md`, `docs/oracle.md` and `docs/mssql.md` each already have
  (or, for Oracle, need) a "how far the checks reach into dynamic SQL" note.
  They must agree once this lands.

## Open questions

> Should an *undecidable* dynamic statement (`EXEC(@sql)`,
> `PREPARE … FROM @var`, `EXECUTE IMMEDIATE v_stmt`) be refused when the grant
> carries `read_only` or `block_ddl`?

Refusing closes the class properly and matches the fail-closed reasoning used
everywhere else in the validators. Leaving it preserves compatibility with
applications that legitimately build SQL at runtime — ORMs and migration tools
do this constantly, so the blast radius of refusing is real and is felt by
well-behaved traffic, not just by an attacker.

## Resolved open questions

> Should an *undecidable* dynamic statement (`EXEC(@sql)`,
> `PREPARE … FROM @var`, `EXECUTE IMMEDIATE v_stmt`) be refused when the grant
> carries `read_only` or `block_ddl`?

**Decision: refuse it, but only under those two controls.** Implement it as
follows:

- A dynamic statement whose payload cannot be extracted statically is refused
  when the active grant carries `read_only` **or** `block_ddl` — the two
  controls whose meaning the wrapper defeats. Fail closed: "dbbat cannot tell
  what this runs" must not resolve to "allow" for a grant that says the session
  may not write or may not change schema.
- It is **not** refused for a grant carrying only `block_copy`, nor for a grant
  with no controls at all (full write). Dynamic SQL defeats neither, so
  refusing there would be blast radius bought for nothing. Do not extend the
  refusal to "any control is set".
- The refusal must name its cause — the statement was refused because its
  payload is not statically decidable under a `read_only`/`block_ddl` grant, not
  a generic "write not permitted" — so an operator can tell this class of
  refusal from an ordinary blocked write and knows the fix is a grant without
  those controls.
- This applies to the undecidable forms only. A statically decidable payload is
  extracted and classified as the Implementation section describes, so a benign
  `EXEC('SELECT 1')` / `PREPARE s FROM 'SELECT 1'` stays allowed under a
  `read_only` grant. Pin that in the tests alongside the refusal table.
- The MySQL `EXECUTE <name>` half follows from the same rule: refuse the
  `EXECUTE` of a prepared name whose `PREPARE` was not checkable, under those
  two controls only.
- Document the resulting behaviour in `docs/mysql.md`, `docs/oracle.md` and
  `docs/mssql.md`, which must agree; state explicitly that
  `EXEC(…) AT <linked_server>` is out of scope and unenforced.
