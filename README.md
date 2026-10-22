# Caddy-IP-List Module

This module retrieves an IP list from specified URLs at a defined interval. It is designed to integrate with other modules like `dynamic_client_ip` or `trusted_proxy` in Caddy.

It maintains a persistent on-disk cache of the last successfully fetched IP ranges so that:
- On Caddy restart, if the remote lists are unavailable, the module loads from the cache.
- During refreshes, if the remote lists are down, the module keeps serving the last known good ranges and updates the cache when sources recover.

Supported from Caddy v2.6.3 onwards.

## Installation

There are two ways to install `caddy-ip-list` with `dynamic_client_ip` support:

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

## Example Configuration

### Using `dynamic_client_ip`

You can get `dynamic_client_ip` from [here](https://github.com/tuzzmaniandevil/caddy-dynamic-clientip)

```caddy
@denied dynamic_client_ip list {
    url https://www.cloudflare.com/ips-v4  # specify the URL to fetch the IP list
    url https://www.cloudflare.com/ips-v6  # You can use multiple URLs
    interval 12h
    timeout 15s
    retries 2
    # Optional: override cache file path (defaults under Caddy data dir)
    # cache_file /var/lib/caddy/ip-list-cache.json
}
abort @denied
```

### Reading a local file instead of a URL

A `file` source is read with no TLS, no DNS and no HTTP round trip, so it can be refreshed much more
often than a URL. The format is identical to the served list: one CIDR per line, `#` starts a comment.

```caddy
@denied dynamic_client_ip list {
    file /srv/whitelist/mono.txt
    interval 30s
}
abort @denied
```

`url` and `file` may be combined in one block; the prefixes are concatenated.

### Using `trusted_proxy`

```caddy
trusted_proxies list {
    url https://www.cloudflare.com/ips-v4  # specify the URL to fetch the IP list
    url https://www.cloudflare.com/ips-v6  # You can use multiple URLs
    interval 12h
    timeout 15s
    retries 2
    # cache_file /var/lib/caddy/ip-list-cache.json
}
```

## Defaults

| Name     | Description                                      | Type     | Default    |
| -------- | ------------------------------------------------ | -------- | ---------- |
| url        | URL(s) to retrieve the IP list                   | string   | *required if no `file`* |
| file       | Local path(s) to read the IP list from           | string   | none       |
| interval   | Frequency at which the IP list is retrieved      | duration | 1h         |
| timeout    | Maximum time to wait for a response from the URL | duration | no timeout |
| retries    | Maximum number of retries per URL per refresh    | int      | 2          |
| cache_file | Optional path for persistent cache               | string   | auto       |
| allow_empty | Accept a refresh that returns no prefixes       | bool     | false      |

## URL Fetching, Caching, and Startup Behavior

- Provisioning performs **no network I/O**: the last known good ranges are loaded from the persistent
  cache, so a Caddy config change is never delayed by a slow or unreachable source.
- On a cold start (no usable cache) the module starts with an empty list and refreshes in the background
  immediately; otherwise the first refresh happens at `interval`.
- When refresh attempts fail, the currently loaded ranges remain in use; once a refresh succeeds, the
  in-memory list and cache are updated.
- A refresh that succeeds but returns **no prefixes** is ignored while a non-empty list is loaded,
  unless `allow_empty` is set. In a `not dynamic_client_ip` matcher an empty list matches everyone, so
  a truncated or emptied source would lock all clients out; keeping the old list fails closed for the
  source but open for the listed clients. Set `allow_empty` only for a list that is genuinely meant
  to become empty.
- The cache file name is derived from every source in the block (URLs and `file` paths), so two blocks
  with different lists never share a cache file.
- The refresh loop runs in the background and is stopped when the configuration is unloaded, so repeated
  config loads do not leak goroutines or timers.

> Note: without a usable cache file, IP range matchers see an empty list until the first background refresh
> completes. Keep the cache on persistent storage (the default under Caddy's data directory does this).