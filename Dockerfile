FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /pool-scaler ./cmd/pool-scaler

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /pool-scaler /pool-scaler
USER 65532:65532
ENTRYPOINT ["/pool-scaler"]
