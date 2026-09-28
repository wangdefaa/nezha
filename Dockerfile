# syntax=docker/dockerfile:1
# 运行镜像只装入 release 流程构建好的静态二进制 dist/dashboard-<os>-<arch>，不在镜像里编译。
# 证书与时区数据和架构无关，在构建机的原生平台准备；最终阶段没有 RUN，构建多架构镜像不需要 QEMU。
FROM --platform=$BUILDPLATFORM alpine AS depend
RUN apk add --no-cache ca-certificates tzdata

FROM busybox:stable-musl

ARG TARGETOS
ARG TARGETARCH

COPY --from=depend /etc/ssl/certs /etc/ssl/certs
COPY --from=depend /usr/share/zoneinfo /usr/share/zoneinfo
COPY --chmod=755 script/entrypoint.sh /entrypoint.sh

WORKDIR /dashboard
COPY --chmod=755 dist/dashboard-${TARGETOS}-${TARGETARCH} ./app

VOLUME ["/dashboard/data"]
EXPOSE 8008
ARG TZ=Asia/Shanghai
ENV TZ=$TZ
ENTRYPOINT ["/entrypoint.sh"]
