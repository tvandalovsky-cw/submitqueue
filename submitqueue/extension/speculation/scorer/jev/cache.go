// Copyright (c) 2026 Uber Technologies, Inc.
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

package jev

import "sync"

// Cache holds model prices by queue and batch ID. It is safe for concurrent
// use and is meant to be shared by every scorer built from one configuration:
// the wiring builds a scorer per speculation run, so a per-scorer cache would
// never be hit.
//
// When full, the oldest entry is evicted. A batch's requests are fixed at
// creation, so a price never goes stale; eviction exists only to bound memory.
type Cache struct {
	mu      sync.Mutex
	max     int
	prices  map[string]float64
	order   []string
	nextOut int
}

// NewCache returns a Cache holding at most maxEntries prices. A non-positive
// maxEntries is treated as 1.
func NewCache(maxEntries int) *Cache {
	if maxEntries < 1 {
		maxEntries = 1
	}
	return &Cache{
		max:    maxEntries,
		prices: make(map[string]float64, maxEntries),
		order:  make([]string, 0, maxEntries),
	}
}

func (c *Cache) get(key string) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.prices[key]
	return p, ok
}

func (c *Cache) put(key string, price float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.prices[key]; ok {
		c.prices[key] = price
		return
	}
	if len(c.order) < c.max {
		c.order = append(c.order, key)
	} else {
		delete(c.prices, c.order[c.nextOut])
		c.order[c.nextOut] = key
		c.nextOut = (c.nextOut + 1) % c.max
	}
	c.prices[key] = price
}
