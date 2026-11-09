# syntax=docker/dockerfile:1
# OpenPak Minecraft service: the Wii U leg (UDP 22960), its internal HTTP surface (20270) and the Java Edition 1.8.9 server it
# bridges to. The server jar is fetched on first start (the Minecraft EULA does not allow shipping it in an image).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.version=${VERSION}" -o /out/minecraft ./cmd/minecraft

FROM public.ecr.aws/docker/library/eclipse-temurin:8-jre
COPY --from=build /out/minecraft /minecraft
ENV MINECRAFT_JAVA_DIR=/data
VOLUME /data
EXPOSE 20270 22960/udp
HEALTHCHECK --start-period=60s CMD ["/minecraft", "healthcheck"]
ENTRYPOINT ["/minecraft"]
