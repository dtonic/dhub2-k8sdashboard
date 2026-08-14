// 집계용 SQL 경로입니다. PromQL 경로와 **의도적으로 분리**되어 있습니다. (#6)
//
// PromQL은 화면 시계열 전용이고, 토폴로지 집계처럼 조인·그룹핑이 필요한 조회는
// SQL로 갑니다. 어느 쪽이든 질의문은 서버 안에서만 만들어집니다 —
// 이 타입을 핸들러에 직접 노출하지 않고, 어댑터 메서드 뒤에 둡니다.
package greptime

import (
	"context"
	"net/url"
)

// SQLQuerier는 집계용 SQL 실행 인터페이스입니다.
// 테스트에서 가짜로 바꿀 수 있도록 구현(sqlClient)과 분리합니다.
type SQLQuerier interface {
	Query(ctx context.Context, sql string) (SQLResult, error)
}

// SQLResult는 GreptimeDB /v1/sql 응답을 최소로 정규화한 표입니다.
type SQLResult struct {
	Columns []string
	Rows    [][]any
}

// SQL은 이 어댑터의 SQL 실행기입니다.
func (s *Source) SQL() SQLQuerier { return sqlClient{s} }

type sqlClient struct{ s *Source }

// greptimeSQLResponse는 /v1/sql 응답 형식입니다.
type greptimeSQLResponse struct {
	Output []struct {
		Records struct {
			Schema struct {
				ColumnSchemas []struct {
					Name string `json:"name"`
				} `json:"column_schemas"`
			} `json:"schema"`
			Rows [][]any `json:"rows"`
		} `json:"records"`
	} `json:"output"`
}

func (c sqlClient) Query(ctx context.Context, sql string) (SQLResult, error) {
	q := url.Values{}
	q.Set("db", c.s.cfg.DB)
	q.Set("sql", sql)

	var res greptimeSQLResponse
	if err := c.s.client.GetJSON(ctx, "/v1/sql", q, &res); err != nil {
		return SQLResult{}, err
	}
	out := SQLResult{}
	if len(res.Output) == 0 {
		return out, nil
	}
	rec := res.Output[0].Records
	for _, col := range rec.Schema.ColumnSchemas {
		out.Columns = append(out.Columns, col.Name)
	}
	out.Rows = rec.Rows
	return out, nil
}
