# gitdisplay

Self-hosted SVG cards for a GitHub profile README: most-used languages, and
contribution streak. Go 1.26, standard library only, no third-party modules.

```
┌─ Most Used Languages ────────────────────────────────┐
│  Active                     Archived                 │
│  Go            65.63%       JavaScript      70.22%   │
│  JavaScript    13.21%       PHP             12.79%   │
│  TypeScript     8.30%       Go              12.27%   │
│  SQL            4.00%       TypeScript       3.57%   │
│  ...                        ...                      │
└──────────────────────────────────────────────────────┘
```

## Why this exists

I have been writing code for about two decades, and most of it has ended up on
GitHub. That turns out to be a problem for the tools that summarise it.

The card I used before — [github-readme-stats][grs], which is excellent and
which I ran happily for years — asks GitHub for `repositories(first: 100)` and
stops there. There is no second page. For most people that is invisible,
because most people do not have more than a hundred repositories.

I have 111 personal repositories, and another 54 across the organisations I
work in. So eleven of mine were being silently dropped — and because GitHub
returns them in creation order, the eleven being dropped were *the newest
ones*. My card was describing the developer I was a decade ago and quietly
ignoring the one I am now. An entire language, Kotlin, never appeared on it at
all, because the only repository I use it in happens to sit at position 102.

The second problem is what twenty years does to a repository list. Projects get
finished, replaced, abandoned, or superseded. I archive them, which is the
whole point of archiving — it says *this is done, I am not working on it*. But
the card treated a WordPress site I stopped touching in 2016 exactly the same
as the Go service I deployed last week. Averaged together, they produced a
number that described neither. My "most used language" was really "the language
I wrote the most bytes of at some point since 2006."

So this card splits them. **Active** repositories on the left, **archived** on
the right, each ranked and given percentages against itself. It reads as a
career arc rather than a lifetime average:

| | Then (archived) | Now (active) |
| --- | --- | --- |
| JavaScript | 70.22% | 9.84% |
| PHP | 12.79% | *not in the top ten* |
| Go | 12.27% | **68.00%** |
| SQL | 0.09% | 4.19% |

That is a picture I recognise. The old card was not wrong, exactly — it was
answering a question nobody asked.

The rest of the problems were smaller but the same shape: things quietly
producing a plausible-looking number that was not true.

- **Purchased and vendored code counted as mine.** One WordPress theme I bought
  was 67% of my total PHP. Three copies of a stock Mono runtime file, bundled
  inside a Unity build that got committed to a repository, produced an entire
  "ASP.NET" row for a language I have never written.
- **A typo silently disabled an exclusion.** One entry in my exclusion list
  had a stray double space in the middle of the repository name, pasted badly
  into a URL years ago. It matched nothing, so it did nothing, and nothing
  told me.
- **Four contributions from 2013 were being thrown away.** The streak card
  clamped the first contribution year to the account creation year unless it
  was before 2005. I made my account in 2014 but imported older history into
  it. Those four commits are real and they are mine.

None of that is a criticism of the original projects. They are free, they are
widely used, and they solve the common case well. I just stopped being the
common case.

[grs]: https://github.com/anuraghazra/github-readme-stats
[grss]: https://github.com/DenverCoder1/github-readme-streak-stats

## What it does differently

| | Original | gitdisplay |
| --- | --- | --- |
| Repositories read | first 100 | all of them, paged |
| Languages per repo | 10 | 50 |
| Organisations | no | yes, listed explicitly |
| Archived projects | mixed in | separate column, or excluded |
| Contribution years | clamped to account creation | every year GitHub reports |
| Vendored code | counted | excluded by repo, path, or exact byte override |
| A config entry that matches nothing | silent | warned on stderr and in `/healthz` |
| Refresh | on request, 3h–24h cache | background goroutine, in-memory serve |

## How it serves

A full refresh is about fifteen GraphQL calls and takes seconds. That never
happens while a request is waiting.

- A background goroutine per card refreshes on a timer and stores the rendered
  SVG bytes in memory. A request is a map lookup and a write — well under a
  millisecond.
- Separate intervals per card.
- **A failed refresh leaves the previous render in place.** An empty, partial,
  or error card is never served into a README.
- Each render is mirrored to disk and read back at start, so a restart or
  reboot serves the last good card immediately instead of a blank one.
- `ETag`, `Last-Modified` and `Cache-Control` are set per card. See
  [doc/CACHING.md](doc/CACHING.md) for why they are what they are, and what
  GitHub's camo proxy does with them.

It serves TLS itself. No reverse proxy, no sidecar. certbot renews the
certificate and the service picks up the new files without a restart.

Endpoints:

| Path | What |
| --- | --- |
| `/top-langs.svg` | languages card |
| `/streak.svg` | streak card |
| `/healthz` | JSON: per-card last successful refresh, last error, config warnings |

## Configuration

Everything lives in a JSON file on the server. Nothing is read from the query
string, so the URL in your README never discloses which repositories you skip.
Start from [`config.example.json`](config.example.json).

Selected settings:

| Key | Meaning |
| --- | --- |
| `timezone` | Explicit IANA name. Day boundaries decide streaks, so this is never taken from the host clock. |
| `languages.split_archived` | The two-column card. |
| `languages.exclude_archived` | Or just drop archived repositories entirely. |
| `languages.organizations` | Organisation logins to include, each queried directly. |
| `languages.exclude_repos` | `owner/repo`, or a bare name. |
| `languages.exclude_paths` | Path prefixes stripped from one repository. |
| `languages.global_exclude_paths` | Path prefixes stripped from every repository. |
| `languages.byte_overrides` | Exact per-repo, per-language byte count. The escape hatch. |
| `languages.language_aliases` | Merge languages, e.g. `TSQL` and `PLpgSQL` into `SQL`. |
| `languages.percent_of_displayed` | Divide by the displayed languages so the card sums to 100%. |
| `streak.exclude_days` | Weekdays that do not break a streak. |
| `streak.start_year` | `0` uses every contribution year GitHub reports. Leave it at `0`. |

### A note on excluding vendored code

GitHub reports one language byte total per repository with no breakdown by
directory, so a directory cannot simply be subtracted. For repositories with a
path exclusion the service reads the git tree, sums blob sizes under each
prefix, works out the language of each file, and takes that off the totals.
Every subtraction is logged and appears in `/healthz`.

Two things worth knowing:

- **`vendor/` and `node_modules/` are already handled.** GitHub's linguist
  leaves them out of a repository's language totals, so listing them changes
  nothing. What this feature is for is directories linguist *does* count and
  you do not want — a purchased theme under `themes/`, a committed build
  output under `dist/`.
- **It approximates linguist, it does not reimplement it.** A subtraction
  larger than the reported total is capped at zero and flagged as `clamped`.
  Where you need an exact number, use `byte_overrides`.

## Command line

```
gitdisplay -config /etc/gitdisplay/config.json      # serve
gitdisplay -config … -once -out ./out              # render both cards to files, exit
gitdisplay -config … -dump data.json               # write the raw API data, exit
gitdisplay -config … -from data.json -once -out .  # render from a dump, no network
gitdisplay -config … -now 2026-07-25 …             # freeze "today" for reproducible output
```

`-dump` captures a fixture; `-from` replays it. Together they let you debug a
card, or reproduce a bug, without touching the API or the running server.

`GITHUB_TOKEN` needs `repo` and `read:user`. The only host contacted is
`api.github.com`.

## Build

Static `linux/arm64` binary, which is what runs in production on a t4g.nano:

```sh
make build
```

or

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty)" \
  -o dist/gitdisplay-linux-arm64 .
```

`CGO_ENABLED=0` removes the libc dependency, so it runs on any arm64 Linux.
Resident set is around 17 MB idle.

## Deploy

`deploy/gitdisplay.service` is the systemd unit. In short:

```sh
sudo install -m 0755 gitdisplay-linux-arm64 /usr/local/bin/gitdisplay
sudo install -d -m 0750 /etc/gitdisplay
sudo install -m 0640 config.json /etc/gitdisplay/config.json
sudo install -m 0600 env /etc/gitdisplay/env          # GITHUB_TOKEN=...
sudo cp deploy/gitdisplay.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now gitdisplay
```

Certificates come from certbot with `--webroot`. The service serves
`/.well-known/acme-challenge/` on port 80 itself, so renewal never needs the
port and never needs the service stopped. certbot's `--deploy-hook` calls
`systemctl reload gitdisplay`, which sends `SIGHUP` and reloads the
certificate with no downtime.

## Behaviour that looks wrong and is not

- **A zero-contribution *today* does not break the current streak.** Without
  this the card reads 0 every morning until the first commit of the day lands.
  A zero on any earlier day does break it.
- **An excluded weekday extends a streak but never starts one.**
- **Yearly contribution windows can overlap at week boundaries.** Days are
  deduplicated by date before summing. In practice GitHub currently clips each
  yearly calendar to the requested range, so no overlap is observed — the
  deduplication is a guard, not a fix for something seen in the data.
- **"Hack" is not a language anyone chose.** GitHub labels a `.php` file with
  no `<?php` opening tag as Hack. When a path exclusion covers `.php` files the
  service reads them to decide which of the two to subtract from.
- **`ASP.NET` and `C#` are different languages to GitHub**, and so are `SQL`,
  `TSQL`, `PLpgSQL` and `PLSQL`. Use `language_aliases` if you would rather
  they shared a row.

## Tests

```sh
go test ./...
```

No test touches the network. Everything runs off a `-dump` fixture, with
`today` frozen to the date the expected numbers were taken from the live cards.

The languages suite pins the *old* behaviour first — first 100 repositories,
ten languages each, the typo left in — and requires it to reproduce the live
card exactly. That proves the pipeline is right before any fix moves a number.
A second test then asserts the corrected output.

## Licence

MIT. See [LICENSE](LICENSE).
