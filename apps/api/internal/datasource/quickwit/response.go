// Elasticsearch 호환 응답 형식입니다. 이 파일 밖에서는 이 구조를 모릅니다.
package quickwit

type esResponse struct {
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []esHit `json:"hits"`
	} `json:"hits"`
	Aggregations struct {
		OverTime struct {
			Buckets []struct {
				Key    float64 `json:"key"`
				Levels struct {
					Buckets []termBucket `json:"buckets"`
				} `json:"levels"`
			} `json:"buckets"`
		} `json:"over_time"`
		Workloads  termBuckets `json:"workloads"`
		Pods       termBuckets `json:"pods"`
		Containers termBuckets `json:"containers"`
	} `json:"aggregations"`
}

type esHit struct {
	ID     string         `json:"_id"`
	Source map[string]any `json:"_source"`
}

type termBucket struct {
	Key      any `json:"key"`
	DocCount int `json:"doc_count"`
}

type termBuckets struct {
	Buckets []termBucket `json:"buckets"`
}
