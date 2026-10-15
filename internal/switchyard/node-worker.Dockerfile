FROM docker.io/library/node@sha256:d6aa754f16b3197301076f047b5def2f02ea1dbbc2ca920407d46d7ec7f87b20

RUN apt-get update -qq \
    && apt-get install -y -qq --no-install-recommends ca-certificates git jq procps \
    && rm -rf /var/lib/apt/lists/* \
    && npm install --global --ignore-scripts --no-audit --no-fund pnpm@10.28.0 \
    && test "$(pnpm --version)" = '10.28.0'
