# Basis-Images: Tag UND Digest (Sweep 2026-09-19). Ein Tag allein floatet — ein
# Rebuild desselben Git-Tags lieferte sonst je nach Tag ein anderes Go-Patchlevel
# und eine andere Alpine-Generation; das nach ghcr gepushte Release-Image war als
# Commit fixiert, als Image nicht. Beim Bump Tag und Digest gemeinsam ändern:
#   docker buildx imagetools inspect <tag> --format '{{.Manifest.Digest}}'
# Die e2e-Toolchain pinnt nach demselben Muster in web/e2e/toolchain.lock.

# Stage 0: Frontend (Svelte 5 + TS + Vite, bun) — produces web/dist for go:embed.
FROM oven/bun:1.4-alpine@sha256:4cf82f5686b196504fe119db3fcd9185b191f82ece82c40680e597228ec6070a AS frontend
WORKDIR /web
# Dependency layer (cache cut like go.mod/go.sum below):
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ .
RUN bun run check && bun run build

FROM golang:1.27-alpine@sha256:d530cf60723abf1ffae0cd16bb546b379ec0550d2f3815e5b16ed69dbc00fba4 AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Real assets ON TOP of the committed placeholder — must happen BEFORE go
# build, go:embed resolves at compile time (web/web.go: //go:embed all:dist).
COPY --from=frontend /web/dist ./web/dist
RUN CGO_ENABLED=0 go build -o /ctx ./cmd/ctxd

FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
COPY --from=build /ctx /ctx
ENV GOMEMLIMIT=200MiB
ENV GOGC=100
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/ctx"]
