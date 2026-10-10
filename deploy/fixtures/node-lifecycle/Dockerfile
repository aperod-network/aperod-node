FROM golang:1.25.13-bookworm AS toolchain
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    systemd systemd-sysv dbus git make binutils gcc libc6-dev \
    python3 python3-yaml curl jq sudo xxd ca-certificates procps util-linux \
    && rm -rf /var/lib/apt/lists/*
COPY --from=toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ENV container=docker CGO_ENABLED=0
COPY deploy /reviewed-deploy
RUN systemctl mask systemd-remount-fs.service getty.target console-getty.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
