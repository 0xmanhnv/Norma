# Third-Party Licenses

Corvid is licensed under the Apache License 2.0 (see [`LICENSE`](LICENSE)). It also
bundles source derived from third-party work that is distributed under its own
license. Those components, and the notices their licenses require, are listed below.

## acp-kernel

The model-driven, archive-backed context-compression engine in Corvid (`fold` /
`foldadapter`) is a Go port of **acp-kernel** by ranxianglei —
<https://github.com/ranxianglei/acp-kernel>. The package was named
`noa` / `noaadapter` before November 2026.

acp-kernel was plain MIT up to and including v0.0.97. From v0.0.98 (upstream commit
`9e726fb`, 2026-09-27) its license is MIT **plus an Additional Term** requiring
attribution on product surfaces. Corvid therefore carries two notices: the MIT license
below for code ported up to v0.0.97, and the
[post-v0.0.98 license](#code-ported-from-v0098-or-later-mit-plus-additional-term) for
anything ported from v0.0.98 or later.

### Code ported up to v0.0.97 (MIT)

The following paths are derived, in whole or in part, from that work:

- `fold/` — the pure compression core. In particular, the four load-bearing
  compression rules in [`fold/prompts_rules.go`](fold/prompts_rules.go) are ported
  verbatim from acp-kernel's `src/compression-rules.ts` (with four deliberate,
  documented deviations noted in that file).
- `foldadapter/` — the adapter that wires the `fold` core into a Corvid session.

The original port tracks acp-kernel v0.0.69. Later fixes were ported from acp-kernel's
own MIT source up to and including v0.0.97 (commit `1d42ec2`). Changes taken from
v0.0.98 or later are listed in the next section. See
[`docs/guides/fold-upstream-sync.md`](docs/guides/fold-upstream-sync.md) for the
per-change record. The synced paths, with the upstream file each derives from:

- `fold/turnintegrity.go` — `ComputeIntegrityWithdrawals` (`src/turn-integrity.ts`).
- `fold/prune.go` — `pairSafeAnchorIndex` (`src/prune.ts`).
- `fold/recommend.go` — `MergeRangesToThreshold`, the foldability screen, block
  boundaries and range positions (`src/recommend.ts`).
- `fold/nudgetext.go` — position-ordered range table and the gentle-nudge tip
  (`src/nudge-text.ts`).
- `fold/compress.go`, `fold/boundaries.go` — gate diagnostics, covering-block naming,
  reversed-ref notes (`src/compress.ts`, `src/boundaries.ts`).
- `fold/protected.go`, `fold/config.go` — `IsNeverPreserveRecent`, the
  `NeverPreserveRecentTools` / `PreserveRecentTools` / `ProtectedLatestTools` knobs
  (`src/protected.ts`, `src/types.ts`); tests in `fold/protection_knobs_test.go`
  adapted from `tests/protected-latest.test.ts`.
- `fold/parseinput.go` — `normalizeSingleQuotes` and `repairAtParserPositions`
  (`src/parse-compress-input.ts`); tests in `fold/parse_salvage_test.go` adapted from
  `tests/basic.test.ts` and `tests/parse-compress-input.test.ts`. acp-kernel's source
  notes that `normalizeSingleQuotes` was itself ported from billion-context
  (`compress-tool.ts`, billion-context #603/#610) — see [billion-context](#billion-context)
  below.
- `fold/prompts_rules.go` — the rule constants, synced to v0.0.97
  (`src/compression-rules.ts`).
- `fold/upstream_sync_test.go` — regression tests adapted from acp-kernel's
  `tests/tool-pair-integrity.test.ts`, `tests/recommend-range-integrity.test.ts`,
  `tests/range-coverage.test.ts` and `tests/regression-bili-470.test.ts`.

These components remain under the MIT License of the original work, as it read up to
v0.0.97. The full license text and its required copyright notice follow:

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

### Code ported from v0.0.98 or later (MIT plus Additional Term)

The following paths contain changes taken from acp-kernel v0.0.98 or later and are
covered by the license below, which is upstream's `LICENSE` as of v0.0.98 (commit
`9e726fb`), unchanged through v0.0.101 (commit `5aa1a10`):

- `fold/nudge.go` — `stampNudge` re-anchors on the shown reference
  (`src/compress.ts`, nudgeNode; `a587c0c`, v0.0.99).
- `fold/parseinput.go` — single entry object in the `content` field
  (`src/parse-compress-input.ts`; `afa1c54`, v0.0.98).
- `fold/const.go`, `fold/refs.go`, `fold/compress.go` — ref capacity widened to
  9,999,999 with hybrid widths (`src/refs.ts`, `src/boundaries.ts`; `83b8609`,
  v0.0.100).
- `fold/prune.go` — `buildAnchorPairingIndex`, built once per prune pass
  (`src/prune.ts`; `78df864`, v0.0.101); tests in `fold/prune_scale_test.go`
  adapted from `tests/prune-recommend-scale.test.ts`.
- `foldadapter/tags.go` — canonical widened ref form in the tag regexes
  (`src/ccr.ts`, `CANONICAL_REF`; `83b8609`); tests in
  `foldadapter/refwidth_test.go`.
- `fold/upstream_v098_test.go` — tests adapted from `tests/nudge.test.ts`,
  `tests/protect-recent.test.ts` (`2cc21fa`, v0.0.100),
  `tests/parse-compress-input.test.ts` and `tests/refs.test.ts`.

The license text, verbatim (English and Chinese):

```
MIT License

Copyright (c) 2026 ranxianglei

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

---

Additional Term — Attribution on Product Surfaces
(在 MIT 之上附加的条款 —— 产品出处标注)

In addition to the conditions above, if you use or incorporate the Software
in a product or service that end users can see or interact with — whether
commercial or open source — you must make attribution clearly visible in at
least one of the following places:

  1. the product's home page;
  2. its documentation; or
  3. its "About" / "Credits" / "Acknowledgements" page.

The attribution must state that the product uses acp-kernel (and
billion-context, where applicable) and include a link to
https://github.com/ranxianglei/acp-kernel (and
https://github.com/ranxianglei/billion-context, where applicable).

For purely server-side or embedded uses with no user-facing surface, carrying
these notices in the shipped documentation satisfies this term.

附加条款（中文）：

除上述条件外，若将本软件用于任何终端用户可见或可交互的产品或服务（无论商业或开源），
须在下述至少一处显著位置标明出处：

  1. 产品首页；
  2. 产品文档；或
  3. “关于 / 致谢 / Acknowledgements”页面。

署名须说明该产品使用了 acp-kernel（涉及时包括 billion-context），并附指向
https://github.com/ranxianglei/acp-kernel （及 https://github.com/ranxianglei/billion-context，
涉及时）的链接。纯服务端或内嵌、无用户可见界面的用途，随附文档中包含上述声明即可。
```

## billion-context

**billion-context** by ranxianglei — <https://github.com/ranxianglei/billion-context> —
is the project acp-kernel was extracted for, and some acp-kernel code originated there.
Corvid takes nothing from billion-context directly; it receives that code only through
acp-kernel, under acp-kernel's license above. It is named here because the acp-kernel
attribution term asks for billion-context to be credited "where applicable", and one
ported function is applicable:

- `fold/parseinput.go` — `normalizeSingleQuotes` (single-quoted Compress argument
  salvage, ported in #22 from acp-kernel v0.0.97). acp-kernel's
  `src/parse-compress-input.ts` records that it was "Ported from billion-context
  compress-tool.ts (#603/#610)".

billion-context's own license is MIT with an equivalent attribution term.

## Downstream obligation

If you build a product or service on Corvid that end users can see or interact with,
and it ships the `fold` / `foldadapter` package, the acp-kernel Additional Term applies
to you too: state, in your product's documentation or on its home page or
About / Credits / Acknowledgements page, that it uses acp-kernel and billion-context,
with links to <https://github.com/ranxianglei/acp-kernel> and
<https://github.com/ranxianglei/billion-context>. For purely server-side or embedded
use, shipping this file with your documentation satisfies the term.
