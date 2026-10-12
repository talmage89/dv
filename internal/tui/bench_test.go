package tui

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/talmage89/dv/internal/testrepo"
)

// BenchmarkFrame measures one full render, which the selection gradient
// repeats about twelve times a second.
func BenchmarkFrame(b *testing.B) {
	r := testrepo.New(b, 10)
	for i := 1; i <= 10; i++ {
		r.Checkout(i, fmt.Sprintf("me/branch-with-a-long-descriptive-name-%d", i))
	}
	a := newTestApp(b)
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		gradientShift += 0.5
		_ = a.View()
	}
}
