# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.0.0
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build \
	-ldflags="-s -w \
	-X 'github.com/ygelfand/posterlink/internal/config.Version=${VERSION}' \
	-X 'github.com/ygelfand/posterlink/internal/config.GitCommit=${GIT_COMMIT}' \
	-X 'github.com/ygelfand/posterlink/internal/config.BuildDate=${BUILD_DATE}'" \
	-o /posterlink .

FROM alpine:3.24
RUN apk add --no-cache ca-certificates && \
	addgroup -S posterlink && adduser -S posterlink -G posterlink
COPY --from=build /posterlink /usr/local/bin/posterlink
USER posterlink
EXPOSE 8088
ENTRYPOINT ["posterlink"]
CMD ["serve"]
