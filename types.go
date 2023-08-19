package main

type Submatches []string

func (sm Submatches) Item(i int) string {
	return sm[i]
}

type TmplContext struct {
	Matches []Submatches
	Type    string
}

func (tc TmplContext) Match(i, j int) string {
	return tc.Matches[i][j]
}
