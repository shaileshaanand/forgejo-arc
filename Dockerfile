FROM golang:1.25.8 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -o /controller ./cmd/controller

FROM gcr.io/distroless/static:nonroot
COPY --from=build /controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
