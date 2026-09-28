package model

import (
	"errors"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 回归：通知发送失败的错误不得带出 URL（常内嵌 bot token），同时保留底层原因便于排障。
func TestNotificationSendErrorOmitsURL(t *testing.T) {
	cases := map[string]string{
		"url.Parse 失败": "https://1.1.1.1:bad/botSECRET-TOKEN/sendMessage",
		"请求头非法":        "https://1.1.1.1/botSECRET-TOKEN/sendMessage",
	}
	for name, rawURL := range cases {
		n := &Notification{URL: rawURL, RequestMethod: NotificationRequestMethodGET, RequestHeader: `{"X":"a\nb"}`}
		err := (&NotificationServerBundle{Notification: n, Loc: time.UTC}).Send("m")
		if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
			t.Fatalf("%s: 错误不得包含 URL 凭据: %v", name, err)
		}
	}
}

func TestRedactURLErrorKeepsCause(t *testing.T) {
	cause := syscall.ECONNREFUSED
	err := redactURLError(&url.Error{Op: "Post", URL: "https://h/botSECRET", Err: cause})
	if strings.Contains(err.Error(), "SECRET") || !errors.Is(err, cause) {
		t.Fatalf("应去掉 URL 且保留底层错误: %v", err)
	}
	if plain := errors.New("x"); redactURLError(plain) != plain {
		t.Fatal("非 url.Error 应原样返回")
	}
}
