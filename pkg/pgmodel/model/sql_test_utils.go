// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.

package model

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgproto3/v2"
	"github.com/jackc/pgtype"
	"github.com/jackc/pgx/v4"
	"github.com/sergi/go-diff/diffmatchpatch"
	"github.com/stretchr/testify/require"
	"github.com/timescale/promscale/pkg/pgmodel/common/errors"
	"github.com/timescale/promscale/pkg/pgmodel/model/pgutf8str"
	"github.com/timescale/promscale/pkg/pgxconn"
)

type SqlRecorder struct {
	queries   []SqlQuery
	nextQuery int
	lock      sync.Mutex
	t         *testing.T
}

type SqlQuery struct {
	Sql           string
	Args          []interface{}
	ArgsUnordered bool
	Results       RowResults
	Err           error
}

// RowResults represents a collection of a multi-column row result
type RowResults [][]interface{}

func NewSqlRecorder(queries []SqlQuery, t *testing.T) *SqlRecorder {
	return &SqlRecorder{queries: queries, t: t}
}

func (r *SqlRecorder) Close() {
}

func (r *SqlRecorder) Exec(ctx context.Context, sql string, arguments ...interface{}) (pgconn.CommandTag, error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	results, err := r.checkQuery(sql, arguments...)

	if len(results) == 0 {
		return nil, err
	}
	if len(results) != 1 {
		r.t.Errorf("mock exec: too many return rows %v\n in Exec\n %v\n args %v",
			results, sql, arguments)
		return nil, err
	}
	if len(results[0]) != 1 {
		r.t.Errorf("mock exec: too many return values %v\n in Exec\n %v\n args %v",
			results, sql, arguments)
		return nil, err
	}

	return results[0][0].(pgconn.CommandTag), err
}

func (r *SqlRecorder) Query(ctx context.Context, sql string, args ...interface{}) (pgxconn.PgxRows, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	rows, err := r.checkQuery(sql, args...)
	return &MockRows{results: rows}, err
}

func (r *SqlRecorder) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	r.lock.Lock()
	defer r.lock.Unlock()
	rows, err := r.checkQuery(sql, args...)
	return &MockRows{results: rows, err: err}
}

func (r *SqlRecorder) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	panic("should never be called")
}

func (r *SqlRecorder) CopyFromRows(rows [][]interface{}) pgx.CopyFromSource {
	panic("should never be called")
}

func (r *SqlRecorder) NewBatch() pgxconn.PgxBatch {
	return &MockBatch{}
}

func (r *SqlRecorder) SendBatch(ctx context.Context, b pgxconn.PgxBatch) (pgx.BatchResults, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	batch := b.(*MockBatch)

	start := r.nextQuery
	for _, q := range batch.items {
		_, _ = r.checkQuery(q.query, q.arguments...)
	}
	// TODO switch to q.query[] subslice
	return &MockBatchResult{queries: r.queries[start:r.nextQuery]}, nil
}

func (r *SqlRecorder) checkQuery(sql string, args ...interface{}) (RowResults, error) {
	idx := r.nextQuery
	if idx >= len(r.queries) {
		r.t.Errorf("@ %d extra query: %s", idx, sql)
		return nil, fmt.Errorf("extra query")
	}
	row := r.queries[idx]
	r.nextQuery += 1

	space := regexp.MustCompile(`\s+`)
	sql = space.ReplaceAllString(sql, " ")
	row.Sql = space.ReplaceAllString(row.Sql, " ")

	if sql != row.Sql {
		dmp := diffmatchpatch.New()
		diffs := dmp.DiffMain(sql, row.Sql, false)
		r.t.Errorf("@ %d unexpected query:\ngot:\n\t'%s'\nexpected:\n\t'%s'\ndiff:\n\t%v", idx, sql, row.Sql, dmp.DiffPrettyText(diffs))
	}

	require.Equal(r.t, len(row.Args), len(args), "Args of different lengths @ %d %s", idx, sql)
	for i := range row.Args {
		switch row.Args[i].(type) {
		case pgtype.TextEncoder:
			ci := pgtype.NewConnInfo()
			got, err := args[i].(pgtype.TextEncoder).EncodeText(ci, nil)
			require.NoError(r.t, err)
			expected, err := row.Args[i].(pgtype.TextEncoder).EncodeText(ci, nil)
			require.NoError(r.t, err)
			require.Equal(r.t, string(expected), string(got), "sql args aren't equal for query # %v: %v", idx, sql)
		default:
			if !row.ArgsUnordered {
				require.Equal(r.t, row.Args[i], args[i], "sql args aren't equal for query # %v: %v", idx, sql)
			} else {
				require.ElementsMatch(r.t, row.Args[i], args[i], "sql args aren't equal for query # %v: %v", idx, sql)
			}
		}
	}
	return row.Results, row.Err
}

type batchItem struct {
	query     string
	arguments []interface{}
}

// Batch queries are a way of bundling multiple queries together to avoid
// unnecessary network round trips.
type MockBatch struct {
	items []batchItem
}

func (b *MockBatch) Queue(query string, arguments ...interface{}) {
	b.items = append(b.items, batchItem{
		query:     query,
		arguments: arguments,
	})
}

type MockBatchResult struct {
	idx     int
	queries []SqlQuery
	t       *testing.T
}

// Exec reads the results from the next query in the batch as if the query has been sent with Conn.Exec.
func (m *MockBatchResult) Exec() (pgconn.CommandTag, error) {
	defer func() { m.idx++ }()

	q := m.queries[m.idx]

	if len(q.Results) == 0 {
		return nil, q.Err
	}
	if len(q.Results) != 1 {
		m.t.Errorf("mock exec: too many return rows %v\n in batch Exec\n %+v", q.Results, q)
		return nil, q.Err
	}
	if len(q.Results[0]) != 1 {
		m.t.Errorf("mock exec: too many return values %v\n in batch Exec\n %+v", q.Results, q)
		return nil, q.Err
	}

	return q.Results[0][0].(pgconn.CommandTag), q.Err
}

// Query reads the results from the next query in the batch as if the query has been sent with Conn.Query.
func (m *MockBatchResult) Query() (pgx.Rows, error) {
	defer func() { m.idx++ }()

	q := m.queries[m.idx]
	return &MockRows{results: q.Results, noNext: false}, q.Err
}

// Close closes the batch operation. This must be called before the underlying connection can be used again. Any error
// that occurred during a batch operation may have made it impossible to resyncronize the connection with the server.
// In this case the underlying connection will have been closed.
func (m *MockBatchResult) Close() error {
	return nil
}

// QueryRow reads the results from the next query in the batch as if the query has been sent with Conn.QueryRow.
func (m *MockBatchResult) QueryRow() pgx.Row {
	defer func() { m.idx++ }()
	q := m.queries[m.idx]
	return &MockRows{results: q.Results, err: q.Err, noNext: false}
}

type MockRows struct {
	idx     int
	noNext  bool
	results RowResults
	err     error
}

// Close closes the rows, making the connection ready for use again. It is safe
// to call Close after rows is already closed.
func (m *MockRows) Close() {
}

// Err returns any error that occurred while reading.
func (m *MockRows) Err() error {
	return m.err
}

// CommandTag returns the command tag from this query. It is only available after Rows is closed.
func (m *MockRows) CommandTag() pgconn.CommandTag {
	panic("not implemented")
}

func (m *MockRows) FieldDescriptions() []pgproto3.FieldDescription {
	panic("not implemented")
}

// Next prepares the next row for reading. It returns true if there is another
// row and false if no more rows are available. It automatically closes rows
// when all rows are read.
func (m *MockRows) Next() bool {
	return !m.noNext && m.idx < len(m.results)
}

// Scan reads the values from the current row into dest values positionally.
// dest can include pointers to core types, values implementing the Scanner
// interface, []byte, and nil. []byte will skip the decoding process and directly
// copy the raw bytes received from PostgreSQL. nil will skip the value entirely.
func (m *MockRows) Scan(dest ...interface{}) error {
	defer func() { m.idx++ }()

	if m.err != nil {
		return m.err
	}

	if m.idx >= len(m.results) {
		return fmt.Errorf("mock scanning error, no more results in batch: got %d wanted %d", m.idx, len(m.results))
	}

	if len(dest) > len(m.results[m.idx]) {
		return fmt.Errorf("mock scanning error, missing results for scanning: got %d %#v\nwanted %d",
			len(m.results[m.idx]),
			m.results[m.idx],
			len(dest),
		)
	}

	for i := range dest {
		switch s := m.results[m.idx][i].(type) {
		case []time.Time:
			if d, ok := dest[i].(*[]time.Time); ok {
				*d = s
			} else if d, ok := dest[i].(pgtype.Value); ok {
				err := d.Set(s)
				if err != nil {
					return err
				}
			} else {
				return fmt.Errorf("wrong value type []time.Time")
			}
		case []float64:
			if d, ok := dest[i].(*[]float64); ok {
				*d = s
			} else if d, ok := dest[i].(pgtype.Value); ok {
				err := d.Set(s)
				if err != nil {
					return err
				}
			} else {
				return fmt.Errorf("wrong value type []float64")
			}
		case []int64:
			if d, ok := dest[i].(*[]int64); ok {
				*d = s
				continue
			}
			if d, ok := dest[i].(*[]SeriesID); ok {
				for _, id := range s {
					*d = append(*d, SeriesID(id))
				}
				continue
			}
			return fmt.Errorf("wrong value type []int64")
		case []int32:
			if d, ok := dest[i].(*[]int32); ok {
				*d = s
				continue
			}
			return fmt.Errorf("wrong value type []int32")
		case []uint8:
			if d, ok := dest[i].(*[]uint8); ok {
				*d = s
				continue
			}
			return fmt.Errorf("wrong value type []int8")
		case []string:
			if d, ok := dest[i].(*[]string); ok {
				*d = s
				continue
			}
			// Ideally, we should be doing pgtype.BinaryDecoder. but doing that here will allow using only
			// a single function that the interface pgtype.BinaryDecoder allows, i.e, DecodeBinary(). DecodeBinary() takes
			// a pgtype.ConnInfo and []byte, which is the main problem. The ConnInfo can be nil, but []byte needs to be set
			// for the DecodeBinary() to work. pgtype.Scan gets the byte slice from values which we do not implement here.
			// Implementing it will require rewriting and changing the [][]interface{}{} to [][]byte and updating all our
			// existing test setup to have the [][]byte (particularly the expected results part), which is lengthy.
			// Plus, not all types convert to [][]byte. types like int will require binary.Little.Endian conversion
			// which can be a overdo for just writing the results of the tests. So, we do a short-cut to directly
			// leverage the .Set() of our custom type and quick the process.
			if d, ok := dest[i].(*pgutf8str.TextArray); ok {
				pgta := pgutf8str.TextArray{}
				if err := pgta.Set(s); err != nil {
					panic(err)
				}
				*d = pgta
				continue
			}
			return fmt.Errorf("wrong value type []string")
		case time.Time:
			if d, ok := dest[i].(*time.Time); ok {
				*d = s
			}
		case float64:
			if _, ok := dest[i].(float64); !ok {
				return fmt.Errorf("wrong value type float64")
			}
			dv := reflect.ValueOf(dest[i])
			dvp := reflect.Indirect(dv)
			dvp.SetFloat(float64(m.results[m.idx][i].(float64)))
		case int:
			if _, ok := dest[i].(*int); !ok {
				return fmt.Errorf("wrong value type int for scan of %T", dest[i])
			}
			dv := reflect.ValueOf(dest[i])
			dvp := reflect.Indirect(dv)
			dvp.SetInt(int64(m.results[m.idx][i].(int)))
		case bool:
			if _, ok := dest[i].(*bool); !ok {
				return fmt.Errorf("wrong value type int for scan of %T", dest[i])
			}
			dv := reflect.ValueOf(dest[i])
			dvp := reflect.Indirect(dv)
			dvp.SetBool(m.results[m.idx][i].(bool))
		case int32:
			if _, ok := dest[i].(*int32); !ok {
				return fmt.Errorf("wrong value type int32")
			}
			dv := reflect.ValueOf(dest[i])
			dvp := reflect.Indirect(dv)
			dvp.SetInt(int64(m.results[m.idx][i].(int32)))
		case uint32:
			if _, ok := dest[i].(*uint32); !ok {
				return fmt.Errorf("wrong value type uint32")
			}
			dv := reflect.ValueOf(dest[i])
			dvp := reflect.Indirect(dv)
			dvp.SetUint(uint64(m.results[m.idx][i].(uint32)))
		case uint8:
			if _, ok := dest[i].(*uint8); !ok {
				return fmt.Errorf("wrong value type uint32")
			}
			dv := reflect.ValueOf(dest[i])
			dvp := reflect.Indirect(dv)
			dvp.SetUint(uint64(m.results[m.idx][i].(uint8)))
		case uint64:
			if _, ok := dest[i].(uint64); !ok {
				return fmt.Errorf("wrong value type uint64")
			}
			dv := reflect.ValueOf(dest[i])
			dvp := reflect.Indirect(dv)
			dvp.SetUint(m.results[m.idx][i].(uint64))
		case int64:
			_, ok1 := dest[i].(*int64)
			_, ok2 := dest[i].(*SeriesID)
			_, ok3 := dest[i].(*SeriesEpoch)
			if !ok1 && !ok2 && !ok3 {
				return fmt.Errorf("wrong value type int64 for scan of %T", dest[i])
			}
			dv := reflect.ValueOf(dest[i])
			dvp := reflect.Indirect(dv)
			dvp.SetInt(m.results[m.idx][i].(int64))
		case string:
			if _, ok := dest[i].(*string); ok {
				dv := reflect.ValueOf(dest[i])
				dvp := reflect.Indirect(dv)
				dvp.SetString(m.results[m.idx][i].(string))
				continue
			}
			if d, ok := dest[i].(*pgutf8str.Text); ok {
				pgt := pgutf8str.Text{}
				if err := pgt.Set(s); err != nil {
					panic(err)
				}
				*d = pgt
				continue
			}
			return fmt.Errorf("wrong value type: neither 'string' or 'pgutf8str'")
		default:
			panic(fmt.Sprintf("unhandled %T", m.results[m.idx][i]))
		}
	}

	return nil
}

// Values returns the decoded row values.
func (m *MockRows) Values() ([]interface{}, error) {
	panic("not implemented")
}

// RawValues returns the unparsed bytes of the row values. The returned [][]byte is only valid until the next Next
// call or the Rows is closed. However, the underlying byte data is safe to retain a reference to and mutate.
func (m *MockRows) RawValues() [][]byte {
	panic("not implemented")
}

type MockMetricCache struct {
	MetricCache  map[string]MetricInfo
	GetMetricErr error
	SetMetricErr error
}

func (m *MockMetricCache) Len() int {
	return len(m.MetricCache)
}

func (m *MockMetricCache) Cap() int {
	return len(m.MetricCache)
}

func (m *MockMetricCache) Evictions() uint64 {
	return 0
}

func (m *MockMetricCache) Get(schema, metric string, isExemplar bool) (MetricInfo, error) {
	if m.GetMetricErr != nil {
		return MetricInfo{}, m.GetMetricErr
	}

	val, ok := m.MetricCache[fmt.Sprintf("%s_%s_%t", schema, metric, isExemplar)]
	if !ok {
		return val, errors.ErrEntryNotFound
	}

	return val, nil
}

func (m *MockMetricCache) Set(schema, metric string, mInfo MetricInfo, isExemplar bool) error {
	m.MetricCache[schema+"*"+metric] = mInfo
	return m.SetMetricErr
}

type MockInserter struct {
	InsertedSeries  map[string]SeriesID
	InsertedData    []map[string][]Insertable
	InsertSeriesErr error
	InsertDataErr   error
}

func (m *MockInserter) Close() {}

func (m *MockInserter) InsertNewData(data Data) (uint64, error) {
	return m.InsertTs(data)
}

func (m *MockInserter) CompleteMetricCreation() error {
	return nil
}

func (m *MockInserter) InsertTs(data Data) (uint64, error) {
	rows := data.Rows
	for _, v := range rows {
		for i, si := range v {
			seriesStr := si.Series().String()
			id, ok := m.InsertedSeries[seriesStr]
			if !ok {
				id = SeriesID(len(m.InsertedSeries))
				m.InsertedSeries[seriesStr] = id
			}
			v[i].Series().seriesID = id
		}
	}
	if m.InsertSeriesErr != nil {
		return 0, m.InsertSeriesErr
	}
	m.InsertedData = append(m.InsertedData, rows)
	ret := 0
	for _, data := range rows {
		for _, si := range data {
			ret += si.Count()
		}
	}
	if m.InsertDataErr != nil {
		ret = 0
	}
	return uint64(ret), m.InsertDataErr
}

func (m *MockInserter) InsertMetadata(metadata []Metadata) (uint64, error) {
	return uint64(len(metadata)), nil
}
