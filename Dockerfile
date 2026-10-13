# ---- build zen-direct (pure stdlib, no deps) ----
FROM golang:1.23-bookworm AS gwbuild
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go build -trimpath -ldflags "-s -w -X main.appVersion=v0.5.1" -o /out/zen-direct .

# ---- runtime: genuine opencode service + gateway, one container ----
FROM node:22-bookworm-slim
RUN npm install -g @opencode/cli@2.0.26 && rm -rf /root/.npm && \
    mkdir -p /data && \
    opencode --version
COPY --from=gwbuild /out/zen-direct /usr/local/bin/zen-direct
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
WORKDIR /data
CMD ["entrypoint.sh"]
