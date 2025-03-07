/*
Copyright SecureKey Technologies Inc. All Rights Reserved.
SPDX-License-Identifier: Apache-2.0
*/

package rollingcounter_test

import (
	"math/rand"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/aries-framework-go-ext/component/vdr/orb/util/concurrent/rollingcounter"
)

func TestCounter(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		counter := rollingcounter.New()

		n := 10
		i := counter.Next(n)
		require.True(t, i < n)

		j := i
		i = counter.Next(n)
		require.True(t, i != j)

		n = 2
		i = counter.Next(n)
		require.True(t, i < n)
	})

	t.Run("Concurrent", func(t *testing.T) {
		counter := rollingcounter.New()

		concurrency := 10
		iterations := 100
		maxN := 10

		var wg sync.WaitGroup
		wg.Add(concurrency)
		defer wg.Wait()

		for g := 0; g < concurrency; g++ {
			go func() {
				defer wg.Done()
				n := rand.Intn(maxN) + 1 //nolint: gosec
				for j := 0; j < iterations; j++ {
					i := counter.Next(n)
					require.True(t, i < n, "index should be less than %d but was %d", n, i)
				}
			}()
		}
	})
}
