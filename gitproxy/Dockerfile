# Static, distroless: this service handles credentials, so it carries no shell
# and no package manager for an attacker to reach.
FROM gcr.io/distroless/static-debian12:nonroot
COPY gitproxy /usr/local/bin/gitproxy
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/gitproxy"]
