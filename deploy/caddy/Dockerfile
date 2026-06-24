# Caddy with the Cloudflare DNS plugin, for wildcard TLS via ACME DNS-01.
# Swap the dns provider module if you use a different DNS host.
FROM caddy:2.10-builder AS build
RUN xcaddy build --with github.com/caddy-dns/cloudflare

FROM caddy:2.10
COPY --from=build /usr/bin/caddy /usr/bin/caddy
