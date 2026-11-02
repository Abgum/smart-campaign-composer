# syntax=docker/dockerfile:1
# One image for every role: api | worker | mockprovider | migrate | seed.
# docker-compose.yml picks the role with the command.

FROM node:22.12.0-alpine AS web
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund --loglevel=error
COPY frontend/ ./
RUN npm run build

FROM golang:1.26.10-bookworm AS build
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/app && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
COPY --from=web /src/frontend/dist /web
# The mock provider's acceptance log lives on a volume mounted here; a new
# named volume takes this directory's owner.
COPY --from=build --chown=65532:65532 /out/data /data
ENV WEB_DIR=/web
USER 65532:65532
ENTRYPOINT ["/app"]
