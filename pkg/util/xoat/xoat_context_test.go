// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package xoat_test

import (
	"context"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vmware-tanzu/vm-operator/pkg/util/xoat"
)

type contextKeyType uint8

const contextKeyValue contextKeyType = 0

var noopExecuteFn = func() {}

var _ = Describe("JoinContext", func() {
	When("parent is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = xoat.JoinContext(nil, xoat.NewContext(contextKeyValue), contextKeyValue)
			}
			Expect(fn).To(PanicWith("parent context is nil"))
		})
	})
	When("contextWithMap is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = xoat.JoinContext(context.Background(), nil, contextKeyValue)
			}
			Expect(fn).To(PanicWith("contextWithMap is nil"))
		})
	})
	When("ctxKey is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = xoat.JoinContext(context.Background(), context.Background(), nil)
			}
			Expect(fn).To(PanicWith("ctxKey is nil"))
		})
	})
	When("map is missing from context", func() {
		It("should panic", func() {
			fn := func() {
				_ = xoat.JoinContext(context.Background(), context.Background(), contextKeyValue)
			}
			Expect(fn).To(PanicWith("map is missing from context"))
		})
	})
	When("the args are correct", func() {
		It("should return a new context", func() {
			ctx := xoat.JoinContext(
				context.Background(),
				xoat.NewContext(contextKeyValue),
				contextKeyValue)
			Expect(ctx).ToNot(BeNil())
		})
	})
})

var _ = Describe("WithContext", func() {
	When("parent is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = xoat.WithContext(nil, contextKeyValue)
			}
			Expect(fn).To(PanicWith("parent context is nil"))
		})
	})
	When("parent is not nil", func() {
		It("should return a context", func() {
			ctx := xoat.WithContext(context.Background(), contextKeyValue)
			Expect(ctx).ToNot(BeNil())
		})
	})
})

var _ = Describe("DeleteChan", func() {
	var (
		ctx   context.Context
		inCtx context.Context
	)

	BeforeEach(func() {
		ctx = xoat.WithContext(context.Background(), contextKeyValue)
		inCtx = ctx
	})

	JustBeforeEach(func() {
		// Create the channels.
		_ = xoat.ExecuteOneAtATime(ctx, contextKeyValue, "fake1", noopExecuteFn)
		_ = xoat.ExecuteOneAtATime(ctx, contextKeyValue, "fake2", noopExecuteFn)

		// Assert the channels exist.
		Expect(xoat.DoesChanExist(ctx, contextKeyValue, "fake1")).To(BeTrue())
		Expect(xoat.DoesChanExist(ctx, contextKeyValue, "fake2")).To(BeTrue())

		xoat.DeleteChan(inCtx, contextKeyValue, "fake1")
		xoat.DeleteChan(inCtx, contextKeyValue, "fake2")
	})

	When("ctx is nil", func() {
		BeforeEach(func() {
			inCtx = nil
		})
		It("should no-op and not delete anything", func() {
			Expect(xoat.DoesChanExist(ctx, contextKeyValue, "fake1")).To(BeTrue())
			Expect(xoat.DoesChanExist(ctx, contextKeyValue, "fake2")).To(BeTrue())
		})
	})

	When("ctx is not nil", func() {
		It("should delete the channels", func() {
			Expect(xoat.DoesChanExist(ctx, contextKeyValue, "fake1")).To(BeFalse())
			Expect(xoat.DoesChanExist(ctx, contextKeyValue, "fake2")).To(BeFalse())
		})
	})

})

var _ = Describe("DoesChanExist", func() {

	var (
		ctx    context.Context
		mapKey any
	)

	BeforeEach(func() {
		ctx = xoat.NewContext(contextKeyValue)
		mapKey = "fake"
	})

	When("ctx is nil", func() {
		It("should panic", func() {
			fn := func() {
				xoat.DoesChanExist(nil, contextKeyValue, mapKey)
			}
			Expect(fn).To(PanicWith("ctx is nil"))
		})
	})

	When("ctxKey is nil", func() {
		It("should panic", func() {
			fn := func() {
				xoat.DoesChanExist(ctx, nil, mapKey)
			}
			Expect(fn).To(PanicWith("ctxKey is nil"))
		})
	})

	When("mapKey is nil", func() {
		It("should panic", func() {
			fn := func() {
				xoat.DoesChanExist(ctx, contextKeyValue, nil)
			}
			Expect(fn).To(PanicWith("mapKey is nil"))
		})
	})

	When("channelMap is missing from context", func() {
		It("should panic", func() {
			fn := func() {
				xoat.DoesChanExist(
					context.Background(),
					contextKeyValue,
					mapKey)
			}
			Expect(fn).To(PanicWith("map is missing from context"))
		})
	})

	When("channel exists", func() {
		It("should return true", func() {
			_ = xoat.ExecuteOneAtATime(
				ctx,
				contextKeyValue,
				mapKey,
				noopExecuteFn)
			Expect(xoat.DoesChanExist(ctx, contextKeyValue, mapKey)).To(BeTrue())
		})
	})
})

var _ = Describe("ExecuteOneAtATime", func() {

	var (
		ctx    context.Context
		mapKey any
	)

	BeforeEach(func() {
		ctx = xoat.NewContext(contextKeyValue)
		mapKey = "fake"
	})

	When("ctx is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = xoat.ExecuteOneAtATime(
					nil,
					contextKeyValue,
					mapKey,
					noopExecuteFn)
			}
			Expect(fn).To(PanicWith("ctx is nil"))
		})
	})

	When("ctxKey is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = xoat.ExecuteOneAtATime(
					ctx,
					nil,
					mapKey,
					noopExecuteFn)
			}
			Expect(fn).To(PanicWith("ctxKey is nil"))
		})
	})

	When("mapKey is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = xoat.ExecuteOneAtATime(
					ctx,
					contextKeyValue,
					nil,
					noopExecuteFn)
			}
			Expect(fn).To(PanicWith("mapKey is nil"))
		})
	})

	When("fn is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = xoat.ExecuteOneAtATime(
					ctx,
					contextKeyValue,
					mapKey,
					nil)
			}
			Expect(fn).To(PanicWith("fn is nil"))
		})
	})

	When("channelMap is missing from context", func() {
		It("should panic", func() {
			fn := func() {
				_ = xoat.ExecuteOneAtATime(
					context.Background(),
					contextKeyValue,
					mapKey,
					noopExecuteFn)
			}
			Expect(fn).To(PanicWith("map is missing from context"))
		})
	})

	When("the context is timed out or cancelled", func() {
		It("should not execute the function", func() {
			var i int64

			fn := func() {
				atomic.AddInt64(&i, 1)
			}

			// Create a cancelled context.
			ctx, cancel := context.WithCancel(ctx)
			cancel()

			// Attempt to increment i, n times.
			for i := 0; i < 5; i++ {
				_ = xoat.ExecuteOneAtATime(
					ctx,
					contextKeyValue,
					mapKey,
					fn)
			}

			Consistently(i, time.Second*3).Should(Equal(int64(0)))
		})
	})

	When("there are multiple concurrent calls for the same namespace/storage class", func() {
		It("should execute the function exactly once", func() {
			var (
				i    int64
				incd = make(chan struct{})
				done = make(chan struct{})
				wait = make(chan struct{})
			)

			fn := func() {
				atomic.AddInt64(&i, 1)

				// Indicate the value has been added.
				close(incd)

				// Wait for the checks to complete before continuing.
				<-wait

				// Indicate that the test may complete.
				close(done)
			}

			// Attempt to increment i multiple times.
			numScheduled := 0
			for j := 0; j < 10; j++ {
				if xoat.ExecuteOneAtATime(
					ctx,
					contextKeyValue,
					mapKey,
					fn) {

					numScheduled++
				}
			}

			Expect(numScheduled).To(Equal(1))

			// Wait for the value to be incremented.
			<-incd

			// Assert that over a period of time, the value is not updated by
			// more than the single goroutine.
			Consistently(i, time.Second*3).Should(Equal(int64(1)))

			// Indicate to the goroutine it can complete.
			close(wait)

			// Wait for the goroutine to finish.
			<-done
		})
	})
})
