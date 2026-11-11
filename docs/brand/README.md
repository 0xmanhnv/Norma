# Brand

corvid's mark is a crow folded from paper. The folds nod to the harness's context
engine, `fold`, which compresses old turns into an archive that can be opened back up.
The crow is the name: corvids are the crow family.

## Files

| File | Use |
|---|---|
| `mark-light.svg`, `mark-dark.svg` | The crow alone, for light and dark backgrounds, at 40 px and up |
| `mark-small-light.svg`, `mark-small-dark.svg` | The crow as one flat shape, for anything under 40 px |
| `wordmark-light.svg`, `wordmark-dark.svg` | The name alone |
| `banner-light.svg`, `banner-dark.svg` | README header (crow, name and tagline) |
| `icon.svg` | Square icon for avatars and favicons; the crow on a dark tile, no legs |
| `social-preview.png` | 1280 × 640 repository social preview |

The README picks the banner that matches the reader's theme with a `<picture>` element.

## Colours

| Role | Light | Dark |
|---|---|---|
| Ink (wordmark) | `#1F2328` | `#E6EDF3` |
| Folds (darkest first) | `#1F2328`, `#2A2F35`, `#353B42`, `#58616C` | `#5A636E`, `#646D78`, `#6E7884`, `#9AA3AD` |
| Beak and the dot on the *i* | `#00ADD8` | `#22C4E8` |
| Beak highlight (55% opacity) | `#7FE3F5` | `#B8F2FC` |
| Eye | `#F2F4F6` | `#E6EDF3` |
| Tagline | `#57606A` | `#8B949E` |
| Background | `#F6F8FA` | `#0D1117` |

`icon.svg` sits on a `#1F2328` tile and uses the dark-theme folds and beak.

Every dark fold is at least 3:1 against the `#0D1117` background, so the whole crow
reads there, not only its lightest fold.

## Small sizes

Below 40 px the four greys run together and the legs disappear. Use the small mark
there: the same outline as one flat shape (`#1F2328` on light, `#9AA3AD` on dark), the
beak in the accent colour, a larger eye, no legs and no highlight. Do not scale the
folded crow down instead, and do not use the small mark where there is room for the
folds.

The beak colour is the one accent. It appears once in the mark (with its highlight)
and once in the wordmark, and nowhere else.

## Wordmark

Lower case, single-weight strokes with round ends, drawn as paths so it renders the same
everywhere without a font. Keep the dot on the *i* in the accent colour. The tagline, when
there is one, is "An agent harness for Go", set in Inter with the system sans-serif
as the fallback.

## Please don't

- recolour the folds or add gradients, outlines or shadows;
- stretch, rotate or mirror the crow (it faces right);
- set the name in another typeface next to the mark;
- use the mark to suggest that a project built on corvid is endorsed by it.
