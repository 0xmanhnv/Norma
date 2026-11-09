# Third-Party Licenses

Norma is licensed under the Apache License 2.0 (see [`LICENSE`](LICENSE)). It also
bundles source derived from third-party work that is distributed under its own
license. Those components, and the notices their licenses require, are listed below.

## acp-kernel (MIT)

The model-driven context-compression engine in Norma is a Go port of **acp-kernel**.
The following paths are derived, in whole or in part, from that work:

- `noa/` — the pure compression core. In particular, the four load-bearing
  compression rules in [`noa/prompts_rules.go`](noa/prompts_rules.go) are ported
  verbatim from acp-kernel's `src/compression-rules.ts` (with four deliberate,
  documented deviations noted in that file).
- `noaadapter/` — the adapter that wires the `noa` core into a Norma session.

The original port tracks acp-kernel v0.0.69. Later fixes were ported from acp-kernel's
own MIT source up to and including v0.0.97 (commit `1d42ec2`); nothing was taken from
v0.0.98 or later. See [`docs/guides/noa-upstream-sync.md`](docs/guides/noa-upstream-sync.md)
for the per-change record. The synced paths, with the upstream file each derives from:

- `noa/turnintegrity.go` — `ComputeIntegrityWithdrawals` (`src/turn-integrity.ts`).
- `noa/prune.go` — `pairSafeAnchorIndex` (`src/prune.ts`).
- `noa/recommend.go` — `MergeRangesToThreshold`, the foldability screen, block
  boundaries and range positions (`src/recommend.ts`).
- `noa/nudgetext.go` — position-ordered range table and the gentle-nudge tip
  (`src/nudge-text.ts`).
- `noa/compress.go`, `noa/boundaries.go` — gate diagnostics, covering-block naming,
  reversed-ref notes (`src/compress.ts`, `src/boundaries.ts`).
- `noa/protected.go`, `noa/config.go` — `IsNeverPreserveRecent`, the
  `NeverPreserveRecentTools` / `PreserveRecentTools` / `ProtectedLatestTools` knobs
  (`src/protected.ts`, `src/types.ts`); tests in `noa/protection_knobs_test.go`
  adapted from `tests/protected-latest.test.ts`.
- `noa/parseinput.go` — `normalizeSingleQuotes` and `repairAtParserPositions`
  (`src/parse-compress-input.ts`); tests in `noa/parse_salvage_test.go` adapted from
  `tests/basic.test.ts` and `tests/parse-compress-input.test.ts`.
- `noa/prompts_rules.go` — the rule constants, synced to v0.0.97
  (`src/compression-rules.ts`).
- `noa/upstream_sync_test.go` — regression tests adapted from acp-kernel's
  `tests/tool-pair-integrity.test.ts`, `tests/recommend-range-integrity.test.ts`,
  `tests/range-coverage.test.ts` and `tests/regression-bili-470.test.ts`.

These components remain under the MIT License of the original work. The full license
text and its required copyright notice follow:

```
MIT License

Copyright © 2026 ranxianglei

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
