# Cache headers and camo

GitHub does not let a README load an image from your server directly. Every
`<img>` in a README is rewritten to a `camo.githubusercontent.com` URL, and
camo fetches the real image on GitHub's side. Two consequences follow.

**Your CDN sees camo, not readers.** Traffic is roughly one fetch per camo
cache miss, not one per person viewing your profile. Response time barely
matters for the reader; freshness does.

**Camo keeps its own copy.** It is a caching proxy you do not control and
cannot purge. Trying to defeat it with `no-cache`, a cache-busting query
string, or a very short `max-age` does not reliably work, and a cache-busting
URL is worse than useless: camo keys on the URL, so a URL that changes every
request means camo never serves a cached copy and every reader waits on a
fresh origin fetch.

## What gets sent

The chain is now camo -> CloudFront -> S3. Each uploaded card carries:

```
Content-Type: image/svg+xml; charset=utf-8
Cache-Control: public, max-age=<n>
```

`max-age` is per card, from `langs_max_age` and `streak_max_age` in the
config. The defaults are one hour for languages and five minutes for the
streak, matching how fast each actually changes.

S3 supplies `ETag` and `Last-Modified` for the object, and CloudFront passes
the headers through, so a revalidation is still cheap: when a cache re-checks
an unchanged card the answer is a `304` with no body. A short `max-age`
therefore costs very little even if camo checks often. This is the opposite
of the `immutable` or multi-day `max-age` that a static asset would want.

## What to actually expect

Setting these headers correctly is worth doing, and it is not a guarantee.
Camo's own behaviour is not documented or contractual, and in practice a
changed card can take longer to appear on a profile than `max-age` suggests.
Treat the headers as the part you control and the propagation delay as
something to live with.

If a card looks stale, fetch the CDN URL directly and compare
`Last-Modified` with the schedule; the function's CloudWatch logs show
whether recent runs succeeded. A stale card with a recent `Last-Modified`
means camo is holding an old copy:

```
curl -sI https://cdn.thesimpledev.com/cards/streak.svg
```

## Why rendering never happens on the request path

A full run is about fifteen GraphQL calls and takes seconds. If that ran
while camo was waiting, a cache miss would be a multi-second stall, and a
GitHub outage would turn into a broken image in the README. Instead the
Lambda function renders on a schedule and uploads static files; a request
only ever touches CloudFront and S3. A failed run uploads nothing, so the
card degrades to slightly stale rather than to blank.
