package websocket

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/boltdb/bolt"
	"github.com/gorilla/websocket"

	"github.com/lian/gdax-bookmap/exchanges/gdax/orderbook"
	"github.com/lian/gdax-bookmap/orderbook/product_info"
	"github.com/lian/gdax-bookmap/util"
)

type Client struct {
	Products    []string
	Books       map[string]*orderbook.Book
	Socket      *websocket.Conn
	DB          *bolt.DB
	dbEnabled   bool
	LastSync    time.Time
	LastDiff    time.Time
	LastDiffSeq uint64
	BatchWrite  map[string]*util.BookBatchWrite
	Infos       []*product_info.Info
}

func New(db *bolt.DB, products []string) *Client {
	c := &Client{
		Products:   []string{},
		Books:      map[string]*orderbook.Book{},
		BatchWrite: map[string]*util.BookBatchWrite{},
		DB:         db,
		Infos:      []*product_info.Info{},
	}
	if c.DB != nil {
		c.dbEnabled = true
	}

	for _, name := range products {
		c.AddProduct(name)
	}

	if c.dbEnabled {
		buckets := []string{}
		for _, info := range c.Infos {
			buckets = append(buckets, info.DatabaseKey)
		}
		util.CreateBucketsDB(c.DB, buckets)
	}

	return c
}

func (c *Client) GetBook(id string) *orderbook.Book {
	return c.Books[id]
}

func (c *Client) AddProduct(name string) {
	c.Products = append(c.Products, name)
	c.Books[name] = orderbook.New(name)
	c.BatchWrite[name] = &util.BookBatchWrite{Count: 0, Batch: []*util.BatchChunk{}}
	info := orderbook.FetchProductInfo(name)
	c.Infos = append(c.Infos, &info)
}

func (c *Client) Connect() error {
	url := "wss://ws-feed.gdax.com"
	fmt.Println("connect to websocket", url)
	s, _, err := websocket.DefaultDialer.Dial(url, nil)

	if err != nil {
		return err
	}

	c.Socket = s

	buf, _ := json.Marshal(map[string]interface{}{"type": "subscribe", "product_ids": c.Products})
	err = c.Socket.WriteMessage(websocket.TextMessage, buf)

	return nil
}

type PacketHeader struct {
	Type      string `json:"type"`
	Sequence  uint64 `json:"sequence"`
	ProductID string `json:"product_id"`
}

func (c *Client) HandleMessage(book *orderbook.Book, header PacketHeader, message []byte) {
	var data map[string]interface{}
	if err := json.Unmarshal(message, &data); err != nil {
		log.Println("HandleMessage:", err)
	}

	var trade *orderbook.Order

	switch header.Type {
	case "received":
		// skip
	case "open":
		price, _ := strconv.ParseFloat(data["price"].(string), 64)
		size, _ := strconv.ParseFloat(data["remaining_size"].(string), 64)

		book.Add(map[string]interface{}{
			"id":    data["order_id"].(string),
			"side":  data["side"].(string),
			"price": price,
			"size":  size,
			//"time":           data["time"].(string),
		})
	case "done":
		book.Remove(data["order_id"].(string))
	case "match":
		price, _ := strconv.ParseFloat(data["price"].(string), 64)
		size, _ := strconv.ParseFloat(data["size"].(string), 64)

		book.Match(map[string]interface{}{
			"size":           size,
			"price":          price,
			"side":           data["side"].(string),
			"maker_order_id": data["maker_order_id"].(string),
			"taker_order_id": data["taker_order_id"].(string),
			"time":           data["time"].(string),
		}, false)
		trade = book.Trades[len(book.Trades)-1]

	case "change":
		if _, ok := book.OrderMap[data["order_id"].(string)]; !ok {
			// if we don't know about the order, it is a change message for a received order
		} else {
			// change messages are treated as match messages
			old_size, _ := strconv.ParseFloat(data["old_size"].(string), 64)
			new_size, _ := strconv.ParseFloat(data["new_size"].(string), 64)
			price, _ := strconv.ParseFloat(data["price"].(string), 64)
			size_delta := old_size - new_size

			book.Match(map[string]interface{}{
				"size":           size_delta,
				"price":          price,
				"side":           data["side"].(string),
				"maker_order_id": data["order_id"].(string),
				//"time":           data["time"].(string),
			}, true)
		}
	}

	if c.dbEnabled {
		batch := c.BatchWrite[book.ID]
		now := time.Now()
		if trade != nil {
			batch.Write(c.DB, now, book.ProductInfo.DatabaseKey, PackTrade(trade))
		}

		if batch.NextSync(now) {
			fmt.Println("STORE SYNC", book.ID, batch.Count)
			c.WriteSync(batch, book, now)
		} else {
			if batch.NextDiff(now) {
				c.WriteDiff(batch, book, now)
			}
		}
	}
}

func (c *Client) WriteDiff(batch *util.BookBatchWrite, book *orderbook.Book, now time.Time) {
	diff := book.Diff
	if len(diff.Bid) != 0 || len(diff.Ask) != 0 {
		pkt := PackDiff(batch.LastDiffSeq, book.Sequence, diff)
		batch.Write(c.DB, now, book.ProductInfo.DatabaseKey, pkt)
		book.ResetDiff()
		batch.LastDiffSeq = book.Sequence + 1
	}
}

func (c *Client) WriteSync(batch *util.BookBatchWrite, book *orderbook.Book, now time.Time) {
	batch.Write(c.DB, now, book.ProductInfo.DatabaseKey, PackSync(book))
	book.ResetDiff()
	batch.LastDiffSeq = book.Sequence + 1
}

func (c *Client) Run() {
	for {
		c.run()
	}
}

func (c *Client) run() {
	if err := c.Connect(); err != nil {
		fmt.Println("failed to connect", err)
		time.Sleep(1000 * time.Millisecond)
		return
	}
	defer c.Socket.Close()

	for {
		msgType, message, err := c.Socket.ReadMessage()
		if err != nil {
			log.Println("read:", err)
			return
		}

		if msgType != websocket.TextMessage {
			continue
		}

		var header PacketHeader
		if err := json.Unmarshal(message, &header); err != nil {
			log.Println("header-parse:", err)
			continue
		}

		var book *orderbook.Book
		var ok bool
		if book, ok = c.Books[header.ProductID]; !ok {
			log.Println("book not found", header.ProductID)
			continue
		}

		if book.Sequence == 0 {
			c.SyncBook(book)
			continue
		}

		if header.Sequence <= book.Sequence {
			// Ignore old messages
			continue
		}

		if header.Sequence != (book.Sequence + 1) {
			// Message lost, resync
			c.SyncBook(book)
			continue
		}

		book.Sequence = header.Sequence

		c.HandleMessage(book, header, message)
	}
}
