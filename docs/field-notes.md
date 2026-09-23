**English** | [한국어](field-notes.ko.md)

# Field notes

[← README](../README.md)

---

In the ten days after 2026-08-03 the development repo (kweiza-cc-plugins) accumulated **759 commits** (115 on August 12 alone).
Below is what the ledger behind the same server measures — query results, not estimates
(2026-08-13 06:40 UTC).

| Axis | Repo A (internal product) | Development repo |
|---|---|---|
| Queue items | 830 | 245 |
| Judgments | 2,015 | 745 |
| Session cards | 331 | 300 |
| Claims | 472 | 240 |
| Landing queue entries | 56 (48 succeeded) | 101 (99 succeeded) |
| **Max sessions holding claims at once** | **7 sessions · 61 items** | 5 sessions · 13 items |
| Max concurrent waiters in the lane | 6 | 2 |

Count by **heartbeat** rather than by claim and the number grows. Sweeping the 17,195 `session.beat`
signals with a sliding window:

| Window | Sessions signalling at once | When |
|---|---|---|
| 5 min | **18** | 2026-08-05 03:07 UTC |
| 10 min | **24** | 2026-08-05 02:33 UTC |

That 24-session moment breaks down as repo A 12 · the development repo 10 · other 2, across 16 distinct worktrees.
**24 sessions ran at once on one machine, and 22 of them were touching the same two repositories.**

601 overlap prescriptions went out, split by reason:

| Prescription | Count | What it said |
|---|---|---|
| `overlap` | 352 | A path you touched intersects that session's footprint |
| `unclaimed` | 124 | You are editing without a claim |
| `silent` | 84 | You have been quiet a long time |
| `outside` | 41 | You are touching paths outside your claimed item |

The whole ledger holds 39,381 events · 2,766 judgments · 3,618 footprint rows.

> **Every one of these numbers is a lower bound.** For two reasons.
>
> ① A failed event write is swallowed as a WARN, and a transaction that fails before it starts never
> even reserves an event. **"0" does not mean "it did not happen"** — do not build a threshold on it.
> ② `claim`'s primary key is `(project, item_id)`, so **re-claiming an item overwrites the previous
> row.** The `claim` table holds 701 rows while `item.claim` events number 734. The concurrency maxima
> above are understated by exactly that much.
