# Container image for CI users who prefer docker run over a binary install:
# the frostfall binary plus a pinned Chromium and the fonts axe-rendered
# pages need. Built and pushed by goreleaser during release; the frostfall
# binary is supplied by goreleaser's build context, not compiled here.
#
# rod's browser discovery finds /usr/bin/chromium-browser (LookPath) and adds
# --no-sandbox automatically when it detects a container, so no wrapper or
# env vars are required.

FROM alpine:3.22

# Renovate keeps this base current; chromium is pinned to the distro release.
RUN apk add --no-cache \
    chromium \
    ca-certificates \
    ttf-freefont \
    font-noto-emoji \
    tzdata

# Run as non-root: chromium refuses some operations as uid 0, and scans have
# no reason to write outside the workdir.
RUN addgroup -S frostfall && adduser -S -G frostfall frostfall
USER frostfall
WORKDIR /work

COPY frostfall /usr/local/bin/frostfall

ENTRYPOINT ["/usr/local/bin/frostfall"]
