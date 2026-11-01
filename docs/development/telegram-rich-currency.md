# Dollar signs in Telegram Rich Messages

Rich Messages render Markdown server-side, including `$…$` math. A message such
as `Paid $10 and $20` can therefore render prose as a formula (and lose bold
markup inside that span). Rich output passes through `parseContent`, which
protects clear numeric currency prefixes with a Markdown backslash.

## Disambiguation policy

- Preserve explicit `$$…$$` display math, including multiline/unfinished blocks.
- Preserve closed inline math: `$x^2$`, `$2+2$`, `$2$`, numeric expressions,
  individual variables, Unicode variables and mathematical operators and LaTeX commands. Closed
  math takes precedence over money; a dollar immediately followed by another
  number is treated as the next price rather than a closing delimiter.
- Otherwise escape a dollar followed by a numeric amount with a prose boundary.
  Decimal commas/points and normal/NBSP/narrow-NBSP grouping are supported.
  Numeric identifiers and unfinished operator expressions are left alone.
- Preserve existing escapes, backtick spans (also multiline), backtick/tilde
  fences, indented code, links/images/reference labels and definitions, and
  HTTP(S) URLs. Shortcut bracket labels are protected only with a
  matching reference definition. Processing is idempotent.

This is intentionally not a full Markdown/TeX parser or universal money detector.
Ambiguous `$2$` stays math; currency inside protected literals/links is not
rewritten. Currency abbreviations attached directly to a number (`$10USD`),
non-numeric dollars and exotic Markdown nesting may remain ambiguous. For
unambiguous currency, authors can use `10 USD` or an explicit `\$10`.

## Delivery paths and verification

Normal sends (also standalone caption overflow) and edits already use
`parseContent`. Rich streaming draft updates and final sends now use it too.
Plain-text fallbacks retain the original unescaped content. HTML/MarkdownV2
conversion is unchanged.

`TestParseContentRichCurrency` includes the original Russian regression and
money/math/code/link/idempotence cases. `TestRichCurrencyDeliveryPaths` checks
serialized Rich Message payloads for send, edit, draft and finalization with a
stub Telegram caller. These are offline tests, **not** proof of Telegram's live
rendering. After approved merge/deployment, inspect an actual message containing
the regression plus `$x^2$`, `$2+2$`, `$2$` and a code span before closing the
runtime incident. Do not restart or deploy merely to run the unit tests.
