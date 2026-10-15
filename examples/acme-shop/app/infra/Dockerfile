FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -o /shop ./cmd/shop

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /shop /shop
COPY web /web
WORKDIR /
EXPOSE 8080
ENTRYPOINT ["/shop"]
