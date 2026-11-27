FROM golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/trick77/ship/kit/version.Version=${VERSION}" -o /out/ovh-restock .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ovh-restock /ovh-restock
USER 65532:65532
WORKDIR /
ENV BACKEND_DATA_DIR=/data BACKEND_CONF_DIR=/conf BACKEND_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/ovh-restock"]
