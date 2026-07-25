# Cache headers and camo

GitHub does not let a README load an image from your server directly. Every
`<img>` in a README is rewritten to a `camo.githubusercontent.com` URL, and
camo fetches the real image on GitHub's side. Two consequences follow.

**Your server sees camo, not readers.** Traffic is roughly one fetch per camo
cache miss, not one per person viewing your profile. Response time barely
matters for the reader; freshness does.

**Camo keeps its own copy.** It is a caching proxy you do not control and
cannot purge. Trying to defeat it with `no-cache`, a cache-busting query
string, or a very short `max-age` does not reliably work, and a cache-busting
URL is worse than useless: camo keys on the URL, so a URL that changes every
request means camo never serves a cached copy and every reader waits on a
fresh origin fetch.

## What this service sends

For each card:

```
Cache-Control: public, max-age=<n>, s-maxage=<n>, must-revalidate
ETag: "<hash of the rendered bytes>"
Last-Modified: <when the render was produced>
Vary: Accept-Encoding
```

`max-age` is per card, from `server.langs_max_age` and
`server.streak_max_age`. The defaults are one hour for languages and five
minutes for the streak, matching how fast each actually changes.

The point of `must-revalidate` together with a strong `ETag` is that a
revalidation is cheap. When camo re-checks, it sends `If-None-Match`; if the
card has not changed the answer is a `304` with no body. So a short `max-age`
costs very little even if camo checks often. This is the opposite of the
`immutable` or multi-day `max-age` that a static asset would want.

The `ETag` is a hash of the rendered SVG, not of the time it was rendered. A
refresh that produces an identical card keeps the same `ETag`, so a
revalidation still returns `304` and nothing is transferred. The `ETag` only
changes when the picture actually changes.

Note the two representations. When a client accepts gzip it gets the
pre-compressed body under an `ETag` with a `-gz` suffix, because a cache
holding one encoding must never answer a revalidation for the other.

## What to actually expect

Setting these headers correctly is worth doing, and it is not a guarantee.
Camo's own behaviour is not documented or contractual, and in practice a
changed card can take longer to appear on a profile than `max-age` suggests.
Treat the headers as the part you control and the propagation delay as
something to live with.

If a card looks stale while `/healthz` shows a recent successful refresh, the
service is fine and camo is holding an old copy. Fetching your own origin URL
directly will confirm which side is stale:

```
curl -sI https://codestats.thesimpledev.com/streak.svg
curl -s https://codestats.thesimpledev.com/healthz | jq '.cards | map_values(.last_success_at)'
```

## Why refreshes never happen on the request path

A full refresh is about fifteen GraphQL calls and takes seconds. If that ran
while camo was waiting, a cache miss would be a multi-second stall, and a
GitHub outage would turn into a broken image in the README. Instead a
background goroutine refreshes on a timer and stores the rendered bytes; a
request is a map lookup and a write. A failed refresh leaves the previous
render in place, so the card degrades to slightly stale rather than to blank.
