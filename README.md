# shadowblade.space

The website supports HTTP/3 over QUIC v2 and v1 with TLS 1.3. The
`Alt-Svc` header advertises HTTP/3 on UDP port 443. HTTP/2 and HTTP/1.1
are available for clients that cannot use QUIC. quic-go version: v0.63.0.

## Visit counter

The `/root/site/poeple.log` file contains only the IP address and a UTC
timestamp (RFC3339), separated by a space. An entry is appended after each
successful `GET /` request, including requests with query parameters and
304 responses. Requests to `/index.html` are redirected to `/` and counted
after the redirect. Requests for assets, HEAD requests, errors, rejected
requests, and requests to subdomains are not logged.

This counts visits served by the server, not unique visitors. Page refreshes
and bots fetching the home page are also counted.

To display the visit count:

```sh
wc -l < /root/site/poeple.log
```

The log has permissions `0600` and is owned by `10001:10001` (the container
user and group). It is mounted as a separate file outside the public
directory and retains its contents when the container is recreated. The
IP address comes from the connection; the `X-Forwarded-For` and `X-Real-IP`
headers cannot override it. Docker requires the file to exist before the
service starts.

## Rate limiting and resource usage

The limits apply only to the static website and are shared across all HTTP versions:

- Home page: 1 request/s per IP, with a burst allowance of 5 requests.
- All paths: 5 requests/s per IP, with a burst allowance of 30 requests.
- Global: 200 requests/s, with a burst allowance of 400 requests.
- Exceeded limits: HTTP 429 with `Retry-After: 1`.
- IPv6: a shared limit per /64 subnet; the full address is retained in the log.
- Rate limiter registry: up to 10,000 entries, with inactive entries removed after 5 minutes.

HTML and assets are loaded into memory and gzip-compressed once at startup.
Assets are cached for 24 hours; HTML requires ETag revalidation so that page
updates and visit counting work on subsequent requests. Only the HTML and
the `assets` directory are included in the list of public files.

QUIC allows 64 concurrent request streams per connection. When incoming
connections exceed 20/s (with a burst allowance of 40), it enables address
validation using Retry. Application-level limits do not protect against
network link saturation; protection against such attacks requires filtering
by the network provider.

## Building and testing

Go 1.26 or later is required. The Docker build uses Go 1.26.8.

```sh
go test -race ./...
docker compose build shadowblade
docker compose up -d --no-deps shadowblade
curl --http3-only -I https://shadowblade.space/
```

## Copyright and license

Copyright (c) 2026 shadow-alpha01. **All rights reserved.**

This project is proprietary. No license is granted to copy, reproduce,
modify, create derivative works from, distribute, publish, sublicense,
sell, or otherwise use the original project materials without the copyright
holder's prior written permission. These restrictions apply to both
commercial and noncommercial use, in whole or in part. See [LICENSE](LICENSE)
for the full terms and applicable exceptions.

Third-party materials retain their own licenses. The bundled IBM Plex Mono
and Space Grotesk fonts are licensed under the SIL Open Font License 1.1;
see [IBM Plex Mono's license](assets/ibm-plex-mono-OFL.txt) and
[Space Grotesk's license](assets/space-grotesk-OFL.txt). External dependencies
remain subject to their respective licenses.
