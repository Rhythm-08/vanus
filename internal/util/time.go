// Copyright 2022 Linkall Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	"context"
	"math"
	"time"
)

const (
	// RFC3339
	vanusTimeLayout = "2006-01-02T15:04:05Z07:00"
)

func FormatTime(t time.Time) string {
	return t.Format(vanusTimeLayout)
}

func ParseTime(str string) (time.Time, error) {
	return time.Parse(vanusTimeLayout, str)
}

func Backoff(attempt int, max time.Duration) time.Duration {
	backoff := float64(100*time.Millisecond) * math.Pow(2, float64(attempt))
	d := time.Duration(backoff)
	if d > max {
		d = max
	}
	return d
}

func Sleep(ctx context.Context, duration time.Duration) bool {
	if duration == 0 {
		select {
		default:
			return true
		case <-ctx.Done():
			return false
		}
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
