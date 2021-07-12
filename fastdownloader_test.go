package main

import (
	"testing"
)

func TestBatchGenerator(t *testing.T) {
	cases := []struct {
		generator func() (uint64, uint64)
		batches   [][]int
	}{
		{
			batchGenerator(uint64(11), uint64(3)),
			[][]int{
				{0, 3},
				{3, 6},
				{6, 9},
				{9, 11},
				{0, 0},
			},
		},
		{
			batchGenerator(uint64(11), uint64(2)),
			[][]int{
				{0, 5},
				{5, 10},
				{10, 11},
				{0, 0},
			},
		},
		{
			batchGenerator(uint64(5), uint64(1)),
			[][]int{
				{0, 5},
				{0, 0},
			},
		},
	}

	for _, testCase := range cases {
		for _, b := range testCase.batches {
			start, stop := testCase.generator()

			if start != uint64(b[0]) || stop != uint64(b[1]) {
				t.Errorf("Failed %d:%d \n", start, stop)
			}
		}
	}
}
