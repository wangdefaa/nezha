#!/bin/sh
# 沿用上游：固定 DNS（Docker 内置解析 + 公共 DNS 兜底），再把 docker run 追加的参数交给面板。
printf "nameserver 127.0.0.11\nnameserver 8.8.4.4\nnameserver 223.5.5.5\n" > /etc/resolv.conf
exec /dashboard/app "$@"
