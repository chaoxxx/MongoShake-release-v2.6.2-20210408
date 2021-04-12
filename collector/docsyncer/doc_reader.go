package docsyncer

import (
	"fmt"
	conf "github.com/alibaba/MongoShake/v2/collector/configure"
	utils "github.com/alibaba/MongoShake/v2/common"
	"math"

	"context"
	"sync/atomic"

	LOG "github.com/vinllen/log4go"
	"github.com/vinllen/mgo"
	"github.com/vinllen/mgo/bson"
	"github.com/vinllen/mongo-go-driver/mongo"
	"github.com/vinllen/mongo-go-driver/mongo/options"
)

/*************************************************/
// splitter: pre-split the collection into several pieces
type DocumentSplitter struct {
	src           string               // source mongo address url
	sslRootCaFile string               // source root ca ssl
	ns            utils.NS             // namespace
	conn          *utils.MongoConn     // connection
	readerChan    chan *DocumentReader // reader chan
	pieceSize     uint64               // each piece max size
	count         uint64               // total document number
	pieceNumber   int                  // how many piece
}

func NewDocumentSplitter(src, sslRootCaFile string, ns utils.NS) *DocumentSplitter {
	ds := &DocumentSplitter{
		src:           src,
		sslRootCaFile: sslRootCaFile,
		ns:            ns,
		pieceSize:     conf.Options.FullSyncReaderReadDocumentCount,
	}

	// create connection
	var err error
	ds.conn, err = utils.NewMongoConn(ds.src, conf.Options.MongoConnectMode, true,
		utils.ReadWriteConcernLocal, utils.ReadWriteConcernDefault, sslRootCaFile)
	if err != nil {
		LOG.Error("splitter[%s] connection mongo[%v] failed[%v]", ds,
			utils.BlockMongoUrlPassword(ds.src, "***"), err)
		return nil
	}

	// get total count
	count, err := ds.conn.Session.DB(ds.ns.Database).C(ds.ns.Collection).Count()
	if err != nil {
		LOG.Error("splitter[%s] connection mongo[%v] failed[%v]", ds,
			utils.BlockMongoUrlPassword(ds.src, "***"), err)
		return nil
	}
	ds.count = uint64(count)

	if ds.pieceSize <= 0 {
		ds.pieceNumber = 1
		ds.readerChan = make(chan *DocumentReader, 1)
	} else {
		ds.pieceNumber = int(math.Ceil(float64(ds.count) / float64(ds.pieceSize)))
		ds.readerChan = make(chan *DocumentReader, SpliterReader)
	}

	go func() {
		if err := ds.Run(); err != nil {
			LOG.Crash(err)
		}
	}()
	return ds
}

func (ds *DocumentSplitter) Close() {
	ds.conn.Close()
}

func (ds *DocumentSplitter) String() string {
	return fmt.Sprintf("DocumentSplitter src[%s] ns[%s] count[%v] pieceSize[%v] pieceNumber[%v]",
		utils.BlockMongoUrlPassword(ds.src, "***"), ds.ns, ds.count, ds.pieceSize, ds.pieceNumber)
}

// TODO, need add retry
func (ds *DocumentSplitter) Run() error {
	// close channel
	defer close(ds.readerChan)

	// disable split
	if ds.pieceNumber == 1 {
		LOG.Info("splitter[%s] disable split or no need", ds)
		ds.readerChan <- NewDocumentReader(ds.src, ds.ns, nil, nil, ds.sslRootCaFile)
		LOG.Info("splitter[%s] exits", ds)
		return nil
	}

	LOG.Info("splitter[%s] enable split: piece size[%v], count[%v]", ds, ds.pieceSize, ds.count)

	var start interface{}
	// cut into piece
	cnt := ds.count
	for i := 0; cnt > 0; i++ {
		result := make(bson.M)
		// current window size
		windowSize := ds.pieceSize
		if cnt < windowSize {
			windowSize = cnt
		}

		query := make(bson.M)
		if start != nil {
			query["_id"] = bson.M{"$gt": start}
		}

		// find the right boundary. the parameter of Skip() is int, what if overflow?
		err := ds.conn.Session.DB(ds.ns.Database).C(ds.ns.Collection).Find(query).Sort("_id").
			Skip(int(windowSize - 1)).Limit(1).One(&result)
		if err != nil {
			return fmt.Errorf("splitter[%s] piece[%d] with query[%v] and skip[%v] fetch boundary failed[%v]",
				ds, i, query, windowSize-1, err)
		}

		end := result["_id"]

		LOG.Info("splitter[%s] piece[%d] create reader with boundary(%v, %v]", ds, i, start, end)
		// inject new DocumentReader into channel
		ds.readerChan <- NewDocumentReader(ds.src, ds.ns, start, end, ds.sslRootCaFile)

		// new start
		start = end
		cnt -= windowSize
	}

	LOG.Info("splitter[%s] exits", ds)
	return nil
}

// @deprecated
func (ds *DocumentSplitter) GetIndexes() ([]mgo.Index, error) {
	return ds.conn.Session.DB(ds.ns.Database).C(ds.ns.Collection).Indexes()
}

/*************************************************/
// DocumentReader: the reader of single piece
type DocumentReader struct {
	// source mongo address url
	src           string
	ns            utils.NS
	sslRootCaFile string // source root ca ssl

	// mongo document reader
	client      *utils.MongoCommunityConn
	docCursor   *mongo.Cursor
	ctx         context.Context
	rebuild     int   // rebuild times
	concurrency int32 // for test only

	// deprecate, used for mgo
	conn        *utils.MongoConn
	docIterator *mgo.Iter

	// query statement and current max cursor
	query bson.M
}

// NewDocumentReader creates reader with mongodb url
func NewDocumentReader(src string, ns utils.NS, start, end interface{}, sslRootCaFile string) *DocumentReader {
	q := make(bson.M)
	if start != nil || end != nil {
		innerQ := make(bson.M)
		if start != nil {
			innerQ["$gt"] = start
		}
		if end != nil {
			innerQ["$lte"] = end
		}
		q["_id"] = innerQ
	}

	ctx := context.Background()

	return &DocumentReader{
		src:           src,
		ns:            ns,
		sslRootCaFile: sslRootCaFile,
		query:         q,
		ctx:           ctx,
	}
}

func (reader *DocumentReader) String() string {
	ret := fmt.Sprintf("DocumentReader src[%v] ns[%s] query[%v]",
		utils.BlockMongoUrlPassword(reader.src, "***"), reader.ns, reader.query)
	if reader.docCursor != nil {
		ret = fmt.Sprintf("%s docCursorId[%v]", ret, reader.docCursor.ID())
	}
	return ret
}

// NextDoc returns an document by raw bytes which is []byte
func (reader *DocumentReader) NextDoc() (doc *bson.Raw, err error) {
	if err := reader.ensureNetwork(); err != nil {
		return nil, err
	}

	atomic.AddInt32(&reader.concurrency, 1)
	defer atomic.AddInt32(&reader.concurrency, -1)

	if !reader.docCursor.Next(reader.ctx) {
		if err := reader.docCursor.Err(); err != nil {
			reader.releaseCursor()
			return nil, err
		} else {
			LOG.Info("reader[%s] finish", reader.String())
			return nil, nil
		}
	}

	doc = new(bson.Raw)
	err = bson.Unmarshal(reader.docCursor.Current, doc)
	if err != nil {
		return nil, err
	}

	/*if conf.Options.LogLevel == utils.VarLogLevelDebug {
		var docParsed bson.M
		bson.Unmarshal(doc.Data, &docParsed)
		LOG.Debug("reader[%v] read doc[%v] concurrency[%d] ts[%v]", reader.ns, docParsed["_id"],
			atomic.LoadInt32(&reader.concurrency), time.Now().Unix())
	}*/
	// err = reader.docCursor.Decode(doc)
	return doc, err
}

// deprecate, used for mgo
func (reader *DocumentReader) NextDocMgo() (doc *bson.Raw, err error) {
	if err := reader.ensureNetworkMgo(); err != nil {
		return nil, err
	}

	atomic.AddInt32(&reader.concurrency, 1)
	defer atomic.AddInt32(&reader.concurrency, -1)

	doc = new(bson.Raw)

	if !reader.docIterator.Next(doc) {
		if err := reader.docIterator.Err(); err != nil {
			// some internal error. need rebuild the oplogsIterator
			reader.releaseIteratorMgo()
			return nil, err
		} else {
			return nil, nil
		}
	}
	return doc, nil
}

// ensureNetwork establish the mongodb connection at first
// if current connection is not ready or disconnected
func (reader *DocumentReader) ensureNetwork() (err error) {
	if reader.docCursor != nil {
		return nil
	}

	if reader.client == nil {
		LOG.Info("reader[%s] client is empty, create one", reader.String())
		reader.client, err = utils.NewMongoCommunityConn(reader.src, conf.Options.MongoConnectMode, true,
			utils.ReadWriteConcernLocal, utils.ReadWriteConcernDefault)
		if err != nil {
			return err
		}
	}

	reader.rebuild += 1
	if reader.rebuild > 1 {
		return fmt.Errorf("reader[%s] rebuild illegal", reader.String())
	}

	findOptions := new(options.FindOptions)
	findOptions.SetSort(map[string]interface{}{
		"_id": 1,
	})
	findOptions.SetBatchSize(8192) // set big for test
	findOptions.SetHint(map[string]interface{}{
		"_id": 1,
	})
	// enable noCursorTimeout anyway! #451
	findOptions.SetNoCursorTimeout(true)
	findOptions.SetComment(fmt.Sprintf("mongo-shake full sync: ns[%v] query[%v] rebuid-times[%v]",
		reader.ns, reader.query, reader.rebuild))

	reader.docCursor, err = reader.client.Client.Database(reader.ns.Database).Collection(reader.ns.Collection, nil).
		Find(nil, reader.query, findOptions)
	if err != nil {
		return fmt.Errorf("run find failed: %v", err)
	}

	LOG.Info("reader[%s] generates new cursor", reader.String())

	return nil
}

// deprecate, used for mgo
func (reader *DocumentReader) ensureNetworkMgo() (err error) {
	if reader.docIterator != nil {
		return nil
	}
	if reader.conn == nil || (reader.conn != nil && !reader.conn.IsGood()) {
		if reader.conn != nil {
			reader.conn.Close()
		}
		// reconnect
		if reader.conn, err = utils.NewMongoConn(reader.src, conf.Options.MongoConnectMode, true,
			utils.ReadWriteConcernLocal, utils.ReadWriteConcernDefault, reader.sslRootCaFile); reader.conn == nil || err != nil {
			return err
		}
	}

	reader.rebuild += 1
	if reader.rebuild > 1 {
		return fmt.Errorf("reader[%s] rebuild illegal", reader.String())
	}

	// rebuild syncerGroup condition statement with current checkpoint timestamp
	reader.conn.Session.SetBatch(8192)
	reader.conn.Session.SetPrefetch(0.2)
	reader.conn.Session.SetCursorTimeout(0)
	reader.docIterator = reader.conn.Session.DB(reader.ns.Database).C(reader.ns.Collection).
		Find(reader.query).Iter()
	return nil
}

func (reader *DocumentReader) releaseCursor() {
	if reader.docCursor != nil {
		LOG.Info("reader[%s] closes cursor[%v]", reader, reader.docCursor.ID())
		err := reader.docCursor.Close(reader.ctx)
		if err != nil {
			LOG.Error("release cursor fail: %v", err)
		}
	}
	reader.docCursor = nil
}

// deprecate, used for mgo
func (reader *DocumentReader) releaseIteratorMgo() {
	if reader.docIterator != nil {
		_ = reader.docIterator.Close()
	}
	reader.docIterator = nil
}

func (reader *DocumentReader) Close() {
	LOG.Info("reader[%s] close", reader)
	if reader.docCursor != nil {
		reader.docCursor.Close(reader.ctx)
	}

	if reader.client != nil {
		reader.client.Client.Disconnect(reader.ctx)
		reader.client = nil
	}
}

// deprecate, used for mgo
func (reader *DocumentReader) CloseMgo() {
	if reader.conn != nil {
		reader.conn.Close()
		reader.conn = nil
	}
}
