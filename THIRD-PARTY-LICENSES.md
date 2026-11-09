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
