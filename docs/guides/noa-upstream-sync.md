# noa upstream sync

> Packages `noa` / `noaadapter` · upstream: [acp-kernel](https://github.com/ranxianglei/acp-kernel) (MIT)

`noa` is a Go port of acp-kernel's compression core. This page records where the port
was taken from, everything upstream changed since, and what Norma did about each
change. It is the reference for the next sync: start from the "pinned" commit below
and walk forward.

## Baseline

| | Commit | Date | Notes |
|---|---|---|---|
| Port baseline | `4e6269d` (v0.0.69) | 2026-09-13 | Upstream `master` when `noa` landed (2026-09-14). |
| Rule constants at port | `17bf3fe` | 2026-09-06 | `src/compression-rules.ts` was unchanged from here to `1c0e579^`; `noa/prompts_rules.go` byte-matched it, modulo the documented deviations. |
| Pinned now | `1d42ec2` (v0.0.97) | 2026-09-27 | Last upstream commit before the license gained an extra term (see below). The four rule constants are byte-identical from here through v0.0.101. |
| Upstream head reviewed | `5aa1a10` (v0.0.101) | 2026-10-03 | 679 commits. |

How the baseline was established: `noa/prompts_rules_test.go` compares the ported
constants with upstream's source. Run against each historical version of
`src/compression-rules.ts`, it passes for `17bf3fe` and fails for `1c0e579` and later.
Feature content narrows it further: `noa` has the array-adjacency range segmentation
(`07ecd94`, 09-07), the per-range user-message counts and block map (`e509583`,
09-11), atomic turns (`645c82d`/`ab5756f`, 09-10) and the usage gate on count-based
tier nudges (`8f2a67d`, 09-09), but none of the 09-15 fixes (`fe26671` onward). The
port was selective: some earlier upstream features were never carried (single-quote
argument salvage 09-07, `protectedLatestTools` 09-09, line-form content 09-13).

The compare test now reads the checkout from `NOA_UPSTREAM_DIR` and skips cleanly when
it is unset:

```sh
git -C ~/src/acp-kernel checkout 1d42ec2
NOA_UPSTREAM_DIR=~/src/acp-kernel go test ./noa -run Rules
```

## Licensing

acp-kernel's own source is MIT (© ranxianglei). Its `PROVENANCE.md` explains that some
files were reimplemented in fresh expression because an ancestor project is AGPL. This
sync reads and ports only acp-kernel's own source and never consults the ancestor.

**The license changed on 2026-09-27** (`9e726fb`, first in v0.0.98). Upstream is now
"MIT plus one additional term": a product with a user-facing surface that uses the
software must attribute acp-kernel, with a link to the repository, on its home page,
in its documentation, or on an About/Credits page. Purely server-side or embedded use
satisfies the term through its shipped documentation.

The first sync (#20–#22) ported nothing from v0.0.98 or later and marked those changes
"blocked: license term" until the owner decided. **The owner has accepted the term**
(#23): the README's Acknowledgements now state that `noa` / `noaadapter` use code from
acp-kernel and billion-context, with both links, and `THIRD-PARTY-LICENSES.md` carries
the post-v0.0.98 license text (MIT plus the Additional Term) next to the original MIT
notice, plus a downstream note: a user-facing product that ships this package must
show the same attribution in its documentation or on an About/Credits page. Code
ported from v0.0.98 or later is listed under the post-v0.0.98 license in that file.

## Changes since the baseline

Legend for **Decision**: *port now* (done in this sync), *port later* (worth having,
not urgent or larger than one PR), *skip* (host/UI-specific, TypeScript-ecosystem,
or not applicable to a Go pure core), *blocked* (after v0.0.97; see Licensing).
**noa had it?** says whether `noa` already had an equivalent before this sync.

### Bug fixes to logic `noa` already ported (highest priority)

| Upstream | What it does | noa had it? | Value / cost / risk | Decision |
|---|---|---|---|---|
| `fe26671` (09-15) fold gate | A fold must never split a tool call from its result. The protected-zone carve can leave the call in the range and the result out of it; the call folds, prune then strips the visible result as an orphan, and that output is silently gone. | No — only the turn (reasoning) invariant. | High / small / low. | **Port now** (`ComputeIntegrityWithdrawals`). |
| `bc0b7ee` (09-16) | Both invariants (turn, pair) re-checked to a fixed point — fixing one can break the other. The recommendation runs the same gate, so it never advertises a range the fold always empties. | No. | High / small / low. | **Port now**. |
| `fe26671` (09-15) prune anchor | A summary placed at its block's earliest message can land between a parallel burst's calls and their results; strict providers reject that. Anchor moves past the result. | No. | High / small / low. | **Port now** (`pairSafeAnchorIndex`). |
| `2f60bfb` (09-15) | A summary landing inside an assistant run splits one provider message; its tool calls lose their reasoning. Anchor moves to the run's start. Applies to Norma: `Reassemble` breaks host messages at synthetic messages. | No. | High / small / low. | **Port now**. |
| `1f35417` (09-16) | `mergeRangesToThreshold` no longer emits a sub-threshold tail batch (guaranteed rejection); it folds into the batch before it or is dropped. | No. | Medium / small / low. | **Port now**. |
| `6996cf6` (09-22) | Batching never bridges an active block: a recommended span over a block sent the model into "already compressed" on a sub-range. | No; and in a pruned view the summary was skipped as unaddressable, so `noa` could not even see the block. | High / small / medium. | **Port now, with a deviation** — see below. |
| `23a7e4e` (09-17) | Ranges carry their view positions; the nudge's range table orders and merges by position, not ref number. | No. | Medium / small / low. | **Port now**. |
| `d2d1c68` (09-19) | "Already compressed" rejection names the covering block, drops a blanket "retry with block ids" hint, and offers tier advice only when it can fire. Protected-exclusion warning tells the model not to re-target the excluded refs. | No (`noa` printed "Active blocks: b1..bN"). | Medium / small / low. | **Port now** (archive path replaces upstream's `decompress` hint). |
| `078d990` (09-16) | Reversed `startId`/`endId` are still swapped, but the swap is reported, including on a rejection. | Swapped silently. | Low-medium / tiny / low. | **Port now**. |
| `4354362` (09-11) | Block map never renders a block-id pair as a span; derive the span from coverage. | Mostly (spans come from coverage) but a fallback could store `bN`. | Low / tiny / low. | **Port now**. |
| `5589da3` (09-24) | Gentle nudge's closing tip no longer demands "compress all ranges" — it contradicted the "if none qualify, continue" advice. | No. | Medium / tiny / low. | **Port now**. |
| `1c0e579` (09-26) prompts | "Open objectives:" carry-forward at every tier, so a still-open user request survives folds and distillations; the summaries-in-context guardrail carves that line out as live tasking. | No. | High / small / low. | **Port now** (rules synced verbatim; guardrail added to `noa`'s own section). |
| `0b94645`, `9509aff` (09-15/16) | Surrogate-safe text clamping. | Go clamps by rune already; one diagnostic (`RawPrefix`) cut by byte. | Low / tiny / low. | **Port now** (rune-safe `RawPrefix`). |
| `6c7930d` (09-24) | Count-triggered tier distillation defaults off (`tier2Trigger` 1000, `tier3Trigger` 2000) and the usage gate on it is deleted: block count is not a need signal, and each distillation rewrites the prompt prefix from the fold anchor on. | `noa` has the usage gate and 5/10 defaults. | Medium / tiny / **behaviour change**. | **Owner decision.** Deleting only the gate would make `noa` more aggressive; changing defaults changes every session's tiering. |
| `3740432`, `8857f2b` | Coverage by base id for `base#sub` projections. | N/A — `noa` ids are content hashes with occurrence suffixes, no sub-ids. | — | Skip. |
| `5a664f5` (09-25) | Snap range endpoints off hidden orphan Compress calls. | N/A — Compress exchanges are hard-protected and never get refs. | — | Skip. |
| `15cad27` (09-20) | Plain ranges skip live host checkpoint carriers. | N/A — `noa` summaries are synthetic, not host messages. | — | Skip. |
| `8a9ffbd`, `cbc78fb` (09-09) | Parse tag-prefixed / stringified Compress call text; stub live summaries. | Yes. Upstream also drops consumed sibling entries of a partly-live batch; `noa` only stubs them (≤200 chars). | Low. | Port later (small token saving). |
| `ab9c229`, `f8a8885` (09-27) | Re-mint live ids that collide with a folded copy. | N/A — the occurrence counter already gives a repeated message its own id. | — | Skip. |
| `a587c0c` (09-28) | Re-anchor nudge cadence when the count drops below the shown reference. | No. | Medium / small / low. | Blocked: license term. |
| `2cc21fa` (09-30) | Case-insensitive protected tool names. | Configured patterns already are; the always-protected list is exact-match on Norma's own name. | Low. | Blocked: license term. |
| `78df864` (10-02) | Build prune/recommend indexes once per turn (performance). | No. | Low-medium. | Blocked: license term. |
| `79f1cba` (09-29) | Linear-time line-form split. | N/A (no line form yet). | — | Blocked: license term. |

**Deviation from `6996cf6`.** Upstream closes a batch at any positional gap, which
includes soft- and hard-protected messages. Ported literally this starved `noa`:
bulky re-obtainable tool output (`NeverPreserveRecentTools`) takes no slot in the
recent zone, so on a read-heavy session the short reasoning and text between reads all
fall inside the zone's token budget, every read becomes a sub-threshold island, and
nothing is ever advertised (`noaadapter`'s long-session simulations caught this). `noa`
closes batches only at active blocks — the case the upstream fix is about — and bridges
protected messages, which `ApplyCompression` excludes with a warning anyway.

### New capabilities

| Upstream | What it does | noa had it? | Value / cost / risk | Decision |
|---|---|---|---|---|
| `8225561`, `2adca91` (09-24/25) | `neverPreserveRecentTools` (replace the default list) and `preserveRecentTools` (subtract from it). The second is the remedy for a fold→re-read loop on freshly read files. | Hard-coded list. | High / small / low. | **Port now.** |
| `4abf11a` (09-09) | `protectedLatestTools`: protect only the newest instance of a cumulative-snapshot tool (e.g. a todo list). | No. | Medium / small / low. | **Port now.** |
| `6b63718` (09-07) | Salvage single-quoted JSON Compress arguments from weak models. | No. | Medium / small / low. | **Port now.** |
| `15ce5ad` (09-13) | Iterative repair at the parser's error position (raw control characters, invalid escapes). | Partly (raw newlines/tabs inside strings). | Medium / small / low. | **Port now.** |
| `1dc17de`, `9f85d55`, `5e5e706`, `9ea8b15`, `1574819`, `1caa488`, `c2066e9` | Line-form content: one string per range, header line `mNNNNN–mNNNNN topic`, body verbatim. Avoids JSON-escaping long summaries. | No. | Medium / medium / medium (new input surface; upstream's follow-up fixes are after v0.0.97). | Port later. |
| `154c834` (09-15) | Terminal-escape signal: after N consecutive turns at the truncation threshold with nothing reclaimable, tell the host the session cannot be saved instead of nudging forever. Also broadens last-resort truncation to oversized text. | No. | High / medium / low (opt-in signal). | Port later — needs a host-facing decision on how `noaadapter` surfaces it. |
| `c84fcad` (09-11) | State-store record codec (compress/encrypt at rest). | No. | Low-medium / small. | Port later (`noaadapter`). |
| `c81aee7` (09-20) | Persistent rules tool (`acp_rule`): short reminders pinned across compressions. | No. | Medium / medium. | Port later. |
| `5d2961a`… (09-25) | Refold-in-place of inline-restored blocks. | N/A — needs a decompress tool; `noa` restores through the archive file. | — | Skip. |
| `40656fa`… (09-23→27) | CCR content store + `acp_retrieve`. | N/A — `noa`'s archive is the content store. | — | Skip. |
| `b8c17c2`, `c6dd2bf` (09-23) | Tool-result valuator and deterministic "crush" plugins (absorb gate). | No. | Medium / large (~1.8k lines). | Port later, as its own project. |
| `397d4b6`, `0d547b3`, `3e643ac`, `35f06bd`, `90588d1`, `035b489` | Image routing/downsampling, historical image stripping, media-preserving wire fixes. | N/A — Norma's `llm` has no image blocks. | — | Skip until Norma carries images. |
| `0bf90ba`, `f127fac`, `51baf8d`, `5a169bc`, `2e32dca` | Prompt-cache reconciliation report. | No. | Low for a library. | Skip (host diagnostics). |
| `7d9487e` (09-23) | Output steering (verbosity / effort routing). | No. | — | Skip (host policy, not compression). |
| `4b803eb`, `f1bd0c1`, `ef67d28`… | Surface-text overrides and prompt packs. | `noa` has `Sections` / `NudgeSections`. | Low. | Skip. |
| `92f37e0`, `56b970f`, `e7f1bb7`, `fd175f4`, `d59de8a` | Status-report surfaces. | N/A — no status tool. | — | Skip. |
| `f6399c4`, `4b3871b` | Search ranking / LRU doc cache. | N/A — `noa` searches by grepping the archive. | — | Skip. |
| `da5f3a4`, `d53b560` | Handoff snapshots. | N/A. | — | Skip. |
| `f3df3eb` (09-25) | Fence async persistence commits. | N/A — `noaadapter` writes synchronously under a mutex. | — | Skip. |
| `a2ee01c`, `ee3d4f1`, `6775cb4`, `40c1f9d` | Wire codecs (Gemini, blank `reasoning_content`, tool-name backfill, malformed bodies). | N/A — `noaadapter` projects `llm.Message` and recovers tool names itself. | — | Skip. |
| `3738dc5`, `bd3f50b`, `b3f5cf0` | Compress schema portability (no top-level combinators; typed `anyOf` alternatives for strict schema validators). | `noa` omits `type` on `content` deliberately. | Low-medium. | Port later, with the line form. |
| `8b8cab6` (10-03) | Preserve `redacted_thinking` blocks across the round trip. | N/A — no such block type in `llm`. | — | Blocked: license term (and N/A). |
| `1eedab9`, `c89994c` (10-02) | Opt-in language-preservation rule (default off; canonical constants unchanged). | No. | Low. | Blocked: license term. |
| `83b8609` (10-02) | Widen refs to 9,999,999 with hybrid widths. | `noa` caps at m99999. | Medium for very long sessions. | Blocked: license term. |
| `afa1c54`, `ffc9fd9`, `e3436c2`, `b871e26`, `7b68d49` | Single-entry object salvage; teach the one-string batch form. | No. | Low. | Blocked: license term. |

## What changed for `noa` users

- **Compression refuses to split a tool call from its result.** A range whose only
  foldable content is half a pair is rejected with a "would split N tool call/result
  pair(s)" error; a larger range compresses the rest and warns. Before, the call folded
  and the visible result was silently stripped.
- **Recommendations are stricter.** A turn split by the protected zone is not offered,
  sub-threshold fragments are not offered, and no batch spans an active block. Fewer
  ranges may appear; each one folds.
- **Summaries move** out of a parallel tool burst or an assistant run. The view stays
  valid; the summary may sit a few positions earlier or later than before.
- **Model-facing text changed**: the "already compressed" rejection, the
  protected-exclusion warning, the gentle-nudge closing tip, a note on reversed refs,
  and the "Open objectives" rule. The resident prompt gained one bullet.
- `RangeInfo` / `ProtectedRange` gained `StartIndex`, `EndIndex`, `Positioned` and
  (`RangeInfo`) `BlockBefore`. Hand-built ranges leave them zero and keep the old
  behaviour.

No configuration default changed.

## Sync log

| PR | Scope |
|---|---|
| #20 | Bug fixes and rules sync (first table above), this document, `NOA_UPSTREAM_DIR`. |
| #21 | `Config.NeverPreserveRecentTools`, `Config.PreserveRecentTools`, `Config.ProtectedLatestTools`. All default to the previous behaviour. |
| #22 | `ParseCompressArgs` salvages single-quoted arguments and repairs control characters / invalid escapes at the decoder's error offset (`Diagnostics.QuoteSalvage`). Also corrects #20's `RawPrefix`: it is capped at 800 **bytes** again (as the fuzz invariant requires), cut at a character boundary. |

`ProtectedLatestTools` deviates in one detail: upstream gives the latest instance a
`BLOCKED` ref, as it does for hard-protected tools. `noa` leaves it addressable and
protects it at recommendation and compression time instead, so once a newer instance
supersedes it, it is an ordinary compressible message with a ref the model can name.
