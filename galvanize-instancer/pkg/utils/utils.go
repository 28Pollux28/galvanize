package utils

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var once sync.Once

var IsDevelopment bool

func Ptr[T any](v T) *T { return &v }

func HTTP500Debug(str string) *string {
	if IsDevelopment {
		return &str
	}
	return Ptr("Internal Server Error")
}

func FormatDuration(d time.Duration) string {
	h := d / time.Hour
	if d%time.Hour == 0 && h > 0 {
		return fmt.Sprintf("%dh", h)
	}

	m := d / time.Minute
	if d%time.Minute == 0 && m > 0 {
		return fmt.Sprintf("%dm", m)
	}

	s := d / time.Second
	if d%time.Second == 0 && s > 0 {
		return fmt.Sprintf("%ds", s)
	}

	return d.String() // fallback for complex durations
}

func init() {
	once.Do(func() {
		dev := os.Getenv("DEVELOPMENT")
		if dev == "true" {
			IsDevelopment = true
		} else {
			IsDevelopment = false
		}
	})
}
