---
name: sql-query-review
description: Review a SQL query for correctness, performance, and safety issues. Use when the user pastes a SQL query and asks for review, feedback, or optimization help.
---
Review the given SQL query for:

1. Correctness — does it do what the user says they want? Point out mismatches
   between stated intent and actual query logic (wrong join type, missing
   WHERE clause, off-by-one in date ranges, etc.).
2. Performance — missing indexes implied by WHERE/JOIN/ORDER BY columns,
   SELECT * where specific columns would do, unnecessary subqueries that could
   be joins, N+1 patterns if this is one of several similar queries.
3. Safety — string-concatenated values that should be parameters, destructive
   statements (UPDATE/DELETE) without a WHERE clause.

Be specific: quote the problematic clause, explain the issue in one sentence,
and give the corrected fragment. Do not rewrite the entire query unless asked.
