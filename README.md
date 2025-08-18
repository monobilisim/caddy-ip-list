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
| url        | URL(s) to retrieve the IP list                   | string   | *required* |
| interval   | Frequency at which the IP list is retrieved      | duration | 1h         |
| timeout    | Maximum time to wait for a response from the URL | duration | no timeout |
| retries    | Maximum number of retries per URL on startup     | int      | 2          |
| cache_file | Optional path for persistent cache               | string   | auto       |

## URL Fetching, Caching, and Startup Behavior

- On startup, the module attempts to fetch each configured URL.
- If fetching fails after `retries`, it will load the last good IP ranges from the persistent cache and continue to start.
- When refresh attempts fail, the currently loaded ranges remain in use; once a refresh succeeds, the in-memory list and cache are updated.
- The refresh loop will continue to update the list in the background at the configured `interval`.