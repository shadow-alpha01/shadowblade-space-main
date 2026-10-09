FROM golang:1.26.8-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go test ./... && \
    CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags "-s -w" -o /server .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10001 web
WORKDIR /srv/www
COPY index.html /srv/www/
COPY assets/ /srv/www/assets/
COPY --from=build /server /usr/local/bin/server
USER web
EXPOSE 8080/tcp 8443/tcp 8443/udp
ENTRYPOINT ["/usr/local/bin/server"]
