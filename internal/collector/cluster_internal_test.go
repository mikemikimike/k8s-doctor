package collector

import (
	"context"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestListAllPages(t *testing.T) {
	pages := map[string]struct {
		items []int
		next  string
	}{"": {[]int{1, 2}, "a"}, "a": {[]int{3}, "b"}, "b": {[]int{4}, ""}}
	var limits []int64
	got, err := listAll(context.Background(), func(_ context.Context, o metav1.ListOptions) ([]int, string, error) {
		limits = append(limits, o.Limit)
		p := pages[o.Continue]
		return p.items, p.next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int{1, 2, 3, 4}) || len(limits) != 3 || limits[0] != pageSize {
		t.Errorf("got %v with limits %v", got, limits)
	}
}
