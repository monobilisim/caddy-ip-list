# Caddy-IP-List Module

This module retrieves an IP list from specified URLs at a defined interval. It is designed to integrate with other modules like `dynamic_client_ip` or `trusted_proxy` in Caddy.

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
}
```

## Defaults

| Name     | Description                                      | Type     | Default    |
| -------- | ------------------------------------------------ | -------- | ---------- |
| url      | URL(s) to retrieve the IP list                   | string   | *required* |
| interval | Frequency at which the IP list is retrieved      | duration | 1h         |
| timeout  | Maximum time to wait for a response from the URL | duration | no timeout |
| retries  | Maximum number of retries per URL on startup     | int      | 0          |

## URL Fetching and Startup Behavior

- On startup, the module will attempt to fetch each configured URL.
- If any URL fails to respond successfully (network error or non-2xx status), Caddy will retry up to the configured `retries` count (with 1s pause between attempts).
- If, after all retries, any URL still fails, Caddy will fail to start, and an error will be logged.
- If all URLs are reachable, the IP list is loaded and used as normal.
- The refresh loop will continue to update the list in the background at the configured `interval`