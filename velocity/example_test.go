package velocity_test

import (
	"fmt"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/velocity"
)

func ExampleStore() {
	s, _ := velocity.New(velocity.Config{})
	t0 := time.Date(2026, 11, 21, 23, 0, 0, 0, time.UTC)
	for i, ben := range []string{"ben-1", "ben-2", "ben-2", "ben-3"} {
		s.Record(velocity.Event{Key: "payer:42", Time: t0.Add(time.Duration(i) * 2 * time.Minute), Amount: 900_00, Counterparty: ben})
	}
	st, _ := s.Query("payer:42", 10*time.Minute, t0.Add(7*time.Minute))
	fmt.Printf("count=%d sum=%d distinct=%d exact=%v\n", st.Count, st.Sum, st.Distinct, st.Exact)
	// Output: count=4 sum=360000 distinct=3 exact=true
}

func ExampleHLL() {
	h, _ := velocity.NewHLL(12)
	for i := 0; i < 100_000; i++ {
		h.AddHash(velocity.Hash64(fmt.Sprint("beneficiary-", i)))
	}
	est := h.Estimate()
	fmt.Printf("within 2%%: %v (σ = %.2f%%)\n", est > 98_000 && est < 102_000, 100*h.StdError())
	// Output: within 2%: true (σ = 1.62%)
}
