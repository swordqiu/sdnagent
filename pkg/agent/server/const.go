// Copyright 2019 Yunion
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

package server

import (
	"time"
)

const (
	GuestCtZoneBase           uint16        = 60000
	FlowManIdleCheckDuration  time.Duration = 13 * time.Second
	TcManIdleCheckDuration    time.Duration = 17 * time.Second
	OvnManRefreshRate         time.Duration = 43 * time.Second
	OvnMdManRefreshRate       time.Duration = 41 * time.Second
	EipManRefreshRate         time.Duration = 43 * time.Second
	WatcherRefreshRate         time.Duration = 31 * time.Second
	WatcherRefreshRateOnError  time.Duration = 1 * time.Second
	WatcherPendingFirstRetry   time.Duration = 300 * time.Millisecond
	WatcherRecentPendingTime   time.Duration = 15 * time.Second
	IfaceJanitorInterval      time.Duration = 57 * time.Second
	TapManRefreshRate         time.Duration = 27 * time.Second
)
