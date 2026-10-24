# [![Contributors][contributors-shield]][contributors-url]
[![Forks][forks-shield]][forks-url]
[![Stargazers][stars-shield]][stars-url]
[![Issues][issues-shield]][issues-url]
[![Apache License][license-shield]][license-url]

[![Readme in English](https://img.shields.io/badge/Readme-English-blue)](README.md)

<div align="center"> 
<a href="https://mono.net.tr/">
  <img src="https://r2.mono.tr/logo/Mono-Logo.svg" width="340"/>
</a>

<h2 align="center">caddy-ip-list</h2>
<b>caddy-ip-list</b> is a Caddy module that provides IP range sources for matchers such as
<a href="https://github.com/tuzzmaniandevil/caddy-dynamic-clientip">dynamic_client_ip</a> and for
<code>trusted_proxies</code>. Lists can come from URLs or from local files, and the last good result is
kept in a persistent on-disk cache so a config load never waits on the network.
</div>

---

## Table of Contents

- [Table of Contents](#table-of-contents)
- [How it works](#how-it-works)
- [Installation](#installation)
- [Configuration](#configuration)
  - [Remote list](#remote-list)
  - [Local file](#local-file)
  - [trusted_proxies](#trusted_proxies)
- [Directives](#directives)
- [Refresh and caching behaviour](#refresh-and-caching-behaviour)
- [Behaviour comparison](#behaviour-comparison)
- [Building](#building)
- [License](#license)

---

## How it works

The module registers the `http.ip_sources.list` module. Every block that uses it keeps its own list in
memory and refreshes it in the background; requests read the current list without any I/O.

- Provisioning performs **no network I/O**. The last good ranges are loaded from the cache file, so a
  `caddy reload` on a large config is not delayed by a slow or unreachable source.
- On a cold start (no usable cache) the list starts empty and is refreshed in the background immediately.
- When a refresh fails, the loaded list stays in use; a successful refresh replaces both the list and the cache.
- A refresh that returns **no prefixes** is ignored while a non-empty list is loaded (see `allow_empty`).

Supported from Caddy v2.6.3 onwards.

---

## Installation

### Build with `xcaddy`

```bash
xcaddy build --with github.com/tuzzmaniandevil/caddy-dynamic-clientip \
             --with github.com/monobilisim/caddy-ip-list
```

### Install via `caddy add-package`

```bash
caddy add-package github.com/tuzzmaniandevil/caddy-dynamic-clientip
caddy add-package github.com/monobilisim/caddy-ip-list
```

---

## Configuration

### Remote list

```caddy
@denied dynamic_client_ip list {
    url https://www.cloudflare.com/ips-v4
    url https://www.cloudflare.com/ips-v6
    interval 12h
    timeout 15s
    retries 2
    # Optional: override the cache file path (defaults under Caddy's data dir)
    # cache_file /var/lib/caddy/ip-list-cache.json
}
abort @denied
```

### Local file

A `file` source needs no TLS, no DNS and no HTTP round trip, so it can be refreshed far more often than a
URL. The format is identical to the served list: one CIDR per line, `#` starts a comment.

```caddy
@denied dynamic_client_ip list {
    file /srv/whitelist/mono.txt
    file /srv/whitelist/local.txt
    interval 30s
}
abort @denied
```

`url` and `file` may be combined in one block; the prefixes are concatenated.

When every source in a block is a `file`, an unchanged file costs one `stat` per tick: the list is not
re-parsed and the cache is not rewritten. This is what makes a short `interval` affordable even when
hundreds of blocks share the same few files.

### `trusted_proxies`

```caddy
trusted_proxies list {
    url https://www.cloudflare.com/ips-v4
    url https://www.cloudflare.com/ips-v6
    interval 12h
    timeout 15s
    retries 2
    # cache_file /var/lib/caddy/ip-list-cache.json
}
```

---

## Directives

| Name | Description | Type | Default |
|---|---|---|---|
| `url` | URL(s) to retrieve the IP list from | string | required if no `file` |
| `file` | Local path(s) to read the IP list from | string | none |
| `interval` | Frequency at which the list is refreshed (capped at `15s` for file-only blocks) | duration | `1h` |
| `timeout` | Maximum time to wait for a response from a URL | duration | no timeout |
| `retries` | Retries per URL per refresh (`0` disables) | int | `2` |
| `cache_file` | Path for the persistent cache | string | derived from the sources |
| `allow_empty` | Accept a refresh that returns no prefixes | bool | `false` |

---

## Refresh and caching behaviour

- **Cache first.** The cache file is named after the sources in the block (`sha256` of the URL list plus
  the `file:` paths), so two blocks with different lists never share a cache file.
- **Failures keep the old list.** A broken source logs a warning and leaves the current list untouched.
- **Empty results keep the old list.** In a `not dynamic_client_ip` matcher an empty list matches every
  client, so a truncated or emptied source would lock everybody out. Unless `allow_empty` is set, an empty
  refresh is discarded while a non-empty list is loaded. Set `allow_empty` only for a list that is genuinely
  meant to become empty.
- **Short intervals are cheap for files.** An unchanged file is detected with `stat` and skipped.
- **File-only blocks ignore long intervals.** When a block has `file` sources and no `url`, `interval` is
  capped at 15s: an unchanged file costs one `stat`, so a 6h interval would only delay picking up a list
  written by CI. Blocks that fetch over the network keep the configured interval.
- **No leaks.** The refresh loop stops when the configuration is unloaded, so repeated config loads do not
  accumulate goroutines or timers.
- **Cold start.** Without a usable cache, matchers see an empty list until the first background refresh
  completes. Keep the cache on persistent storage (the default under Caddy's data directory does this).

> Note: with `not dynamic_client_ip`, an empty list denies everything. Warm the cache on a passive node
> before promoting new sources to production.

---

## Behaviour comparison

`scripts/compare.sh` builds two revisions and runs the same scenarios against both:

```bash
./scripts/compare.sh          # markdown table
./scripts/compare.sh --json   # machine readable
```

| scenario | `9336c17` (before) | current |
|---|---|---|
| source returns 404, no cache | **config rejected, Caddy does not start** | loads, empty list, recovers |
| source returns 404, cache present | loads from cache | loads from cache |
| source returns 200, no cache | loads | loads |

The first row is why a single bad URL used to take a whole config down: `Provision` returned an error and
Caddy discards the entire config when a module fails to provision. Override the revisions with `OLD_REF` /
`NEW_REF`, or point at binaries you already have with `OLD_BIN` / `NEW_BIN`.

---

## Building

```bash
go build ./...
go test ./...
```

To build Caddy with this module:

```bash
xcaddy build --with github.com/monobilisim/caddy-ip-list
```

---

## License

caddy-ip-list is licensed under Apache-2.0. See [LICENSE](LICENSE) file for details.

[contributors-shield]: https://img.shields.io/github/contributors/monobilisim/caddy-ip-list.svg?style=for-the-badge
[contributors-url]: https://github.com/monobilisim/caddy-ip-list/graphs/contributors
[forks-shield]: https://img.shields.io/github/forks/monobilisim/caddy-ip-list.svg?style=for-the-badge
[forks-url]: https://github.com/monobilisim/caddy-ip-list/network/members
[stars-shield]: https://img.shields.io/github/stars/monobilisim/caddy-ip-list.svg?style=for-the-badge
[stars-url]: https://github.com/monobilisim/caddy-ip-list/stargazers
[issues-shield]: https://img.shields.io/github/issues/monobilisim/caddy-ip-list.svg?style=for-the-badge
[issues-url]: https://github.com/monobilisim/caddy-ip-list/issues
[license-shield]: https://img.shields.io/github/license/monobilisim/caddy-ip-list.svg?style=for-the-badge
[license-url]: https://github.com/monobilisim/caddy-ip-list/blob/main/LICENSE
