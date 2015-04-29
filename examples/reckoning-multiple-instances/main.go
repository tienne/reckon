/*
 * Copyright (C) 2015 zulily, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package main

import (
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/zulily/reckon"
)

// reckonResult allow us to return results OR an error on the same chan
type reckonResult struct {
	s        map[string]*reckon.Results
	keyCount int64
	err      error
}

func main() {

	// Sample 100 keys from each of three redis instances, all running on different ports on localhost
	redises := []reckon.Options{
		reckon.Options{Host: "localhost", Port: 6379, MinSamples: 100},
		reckon.Options{Host: "localhost", Port: 6380, MinSamples: 100},
		reckon.Options{Host: "localhost", Port: 6381, MinSamples: 100},
	}

	aggregator := reckon.AggregatorFunc(reckon.AnyKey)

	var wg sync.WaitGroup
	results := make(chan reckonResult)

	wg.Add(len(redises))

	// Sample each redis in its own goroutine
	for _, redis := range redises {
		go func(opts reckon.Options) {
			defer wg.Done()
			log.Printf("Sampling %d keys from redis at: %s:%d...\n", opts.MinSamples, opts.Host, opts.Port)
			s, keyCount, err := reckon.Run(opts, aggregator)
			results <- reckonResult{s: s, keyCount: keyCount, err: err}
		}(redis)
	}

	// Collect and merge all the results
	totals := make(map[string]*reckon.Results)
	totalKeyCount := int64(0)

	go func() {
		for r := range results {
			if r.err != nil {
				panic(r.err)
			}
			log.Println("Got results back from a redis instance!")

			totalKeyCount += r.keyCount
			for k, v := range r.s {
				if existing, ok := totals[k]; ok {
					existing.Merge(v)
					totals[k] = existing
				} else {
					totals[k] = v
				}
			}
		}
	}()

	// render the final results to HTML when everything is complete
	wg.Wait()
	close(results)

	log.Printf("total key count: %d\n", totalKeyCount)
	for k, v := range totals {

		v.Name = k
		if f, err := os.Create(fmt.Sprintf("output-%s.html", k)); err != nil {
			panic(err)
		} else {
			defer f.Close()
			log.Printf("Rendering totals for: %s to %s:\n", k, f.Name())
			if err := reckon.RenderHTML(v, f); err != nil {
				panic(err)
			}
		}

	}
}