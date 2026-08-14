package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

type ScopeIdentity struct {
	Clusters []ClusterIdentity `json:"clusters"`
}
type ClusterIdentity struct {
	ID         string   `json:"id"`
	All        bool     `json:"all"`
	Namespaces []string `json:"namespaces"`
}
type Param struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}
type Identity struct {
	Dashboard   string        `json:"dashboard"`
	QueryRef    string        `json:"queryRef"`
	Scope       ScopeIdentity `json:"scope"`
	Range       string        `json:"range"`
	From        string        `json:"from,omitempty"`
	To          string        `json:"to,omitempty"`
	StepSeconds int64         `json:"stepSeconds"`
	Params      []Param       `json:"params"`
}

func (i Identity) Key() string {
	i.Scope.Clusters = append([]ClusterIdentity(nil), i.Scope.Clusters...)
	for n := range i.Scope.Clusters {
		i.Scope.Clusters[n].Namespaces = append([]string(nil), i.Scope.Clusters[n].Namespaces...)
	}
	i.Params = append([]Param(nil), i.Params...)
	for n := range i.Params {
		i.Params[n].Values = append([]string(nil), i.Params[n].Values...)
	}
	sort.Slice(i.Scope.Clusters, func(a, b int) bool { return i.Scope.Clusters[a].ID < i.Scope.Clusters[b].ID })
	for n := range i.Scope.Clusters {
		sort.Strings(i.Scope.Clusters[n].Namespaces)
	}
	sort.Slice(i.Params, func(a, b int) bool { return i.Params[a].Name < i.Params[b].Name })
	b, _ := json.Marshal(i)
	sum := sha256.Sum256(b)
	return "dashboard:v1:" + hex.EncodeToString(sum[:])
}

type TTLClass int

const (
	State TTLClass = iota
	Short
	Historical
)

type TTLPolicy struct {
	State, Short, Historical time.Duration
	HistoricalSafety         time.Duration
}

func (p TTLPolicy) For(class TTLClass, customTo, now time.Time) time.Duration {
	if class == Historical && !customTo.IsZero() && !customTo.After(now.Add(-p.HistoricalSafety)) {
		return p.Historical
	}
	if class == Short {
		return p.Short
	}
	return p.State
}
