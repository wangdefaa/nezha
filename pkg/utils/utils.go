package utils

import (
	"cmp"
	"crypto/rand"
	"errors"
	"fmt"
	"iter"
	"maps"
	"math/big"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

var (
	ipv4Re = regexp.MustCompile(`(\d*\.).*(\.\d*)`)
	ipv6Re = regexp.MustCompile(`(\w*:\w*:).*(:\w*:\w*)`)
)

func ipv4Desensitize(ipv4Addr string) string {
	return ipv4Re.ReplaceAllString(ipv4Addr, "$1****$2")
}

func ipv6Desensitize(ipv6Addr string) string {
	return ipv6Re.ReplaceAllString(ipv6Addr, "$1****$2")
}

func IPDesensitize(ipAddr string) string {
	ipAddr = ipv4Desensitize(ipAddr)
	ipAddr = ipv6Desensitize(ipAddr)
	return ipAddr
}

func IPStringToBinary(ip string) ([]byte, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return nil, err
	}
	b := addr.As16()
	return b[:], nil
}

func GetIPFromHeader(headerValue string) (string, error) {
	a := strings.Split(headerValue, ",")
	h := strings.TrimSpace(a[len(a)-1])
	ip, err := netip.ParseAddr(h)
	if err != nil {
		return "", err
	}
	if !ip.IsValid() {
		return "", errors.New("invalid ip")
	}
	return ip.String(), nil
}

func GenerateRandomString(n int) (string, error) {
	const letters = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	lettersLength := big.NewInt(int64(len(letters)))
	ret := make([]byte, n)
	for i := range n {
		num, err := rand.Int(rand.Reader, lettersLength)
		if err != nil {
			return "", err
		}
		ret[i] = letters[num.Int64()]
	}
	return string(ret), nil
}

func MustGenerateRandomString(n int) string {
	str, err := GenerateRandomString(n)
	if err != nil {
		panic(fmt.Errorf("MustGenerateRandomString: %v", err))
	}
	return str
}

func IfOr[T any](a bool, x, y T) T {
	if a {
		return x
	}
	return y
}

func MapValuesToSlice[Map ~map[K]V, K comparable, V any](m Map) []V {
	s := make([]V, 0, len(m))
	return slices.AppendSeq(s, maps.Values(m))
}

func MapKeysToSlice[Map ~map[K]V, K comparable, V any](m Map) []K {
	s := make([]K, 0, len(m))
	return slices.AppendSeq(s, maps.Keys(m))
}

func Unique[S ~[]E, E cmp.Ordered](list S) S {
	if list == nil {
		return nil
	}
	out := make([]E, len(list))
	copy(out, list)
	slices.Sort(out)
	return slices.Compact(out)
}

func ConvertSeq[In, Out any](seq iter.Seq[In], f func(In) Out) iter.Seq[Out] {
	return func(yield func(Out) bool) {
		for in := range seq {
			if !yield(f(in)) {
				return
			}
		}
	}
}

func ConvertSeq2[KIn, VIn, KOut, VOut any](seq iter.Seq2[KIn, VIn], f func(KIn, VIn) (KOut, VOut)) iter.Seq2[KOut, VOut] {
	return func(yield func(KOut, VOut) bool) {
		for k, v := range seq {
			if !yield(f(k, v)) {
				return
			}
		}
	}
}

func Seq2To1[K, V any](seq iter.Seq2[K, V]) iter.Seq[V] {
	return func(yield func(V) bool) {
		for _, v := range seq {
			if !yield(v) {
				return
			}
		}
	}
}

type WrapError struct {
	err, errIn error
}

func NewWrapError(err, errIn error) error {
	return &WrapError{err, errIn}
}

func (e *WrapError) Error() string {
	return e.err.Error()
}

func (e *WrapError) Unwrap() error {
	return e.errIn
}

func FirstError(errorer ...func() error) error {
	for _, fn := range errorer {
		if err := fn(); err != nil {
			return err
		}
	}
	return nil
}

// SubUintChecked 返回 a-b，a<b 时返回 0（防无符号下溢，如 agent 重启后流量计数归零）。
func SubUintChecked(a, b uint64) uint64 {
	if a < b {
		return 0
	}

	return a - b
}
