package badcapt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/olivere/elastic"
)

const (
	indexName = "badcapt"
	docType   = "bcrecord"
)

// Marker represents a routine that identifies the raw packet.
type Marker func(gopacket.Packet) []string

// SeriesMarker represents a routine that identifies a series of packets.
type SeriesMarker func(...gopacket.Packet) []string

var defaultMarkers = []Marker{
	MiraiIdentifier,
	ZmapIdentifier,
	MasscanIdentifier,
}

var defaultSeriesMarkers = []SeriesMarker{}

// Badcapt defines badcapt configuration
type Badcapt struct {
	client        *elastic.Client
	indexName     string
	docType       string
	markers       []Marker
	seriesMarkers []SeriesMarker
	cache         *fastcache.Cache
	cacheSize     int
}

// TaggedPacket represents a packet that went through markers.
type TaggedPacket struct {
	Packet gopacket.Packet
	Tags   []string
}

// Record contains packet data, that is ready to be exported
type Record struct {
	SrcIP          net.IP    `json:"src_ip,omitempty"`
	TransportProto string    `json:"transport"`
	SrcPort        uint16    `json:"src_port"`
	DstIP          net.IP    `json:"dst_ip,omitempty"`
	DstPort        uint16    `json:"dst_port"`
	Timestamp      time.Time `json:"date"`
	Tags           []string  `json:"tags"`
	Payload        []byte    `json:"payload,omitempty"`
	PayloadString  string    `json:"payload_str,omitempty"`
}

func unpackIPv4(p gopacket.Packet) *layers.IPv4 {
	ip4Layer := p.Layer(layers.LayerTypeIPv4)
	if ip4Layer == nil {
		return nil
	}
	ip4 := ip4Layer.(*layers.IPv4)

	return ip4
}

func unpackTCP(p gopacket.Packet) *layers.TCP {
	tcpLayer := p.Layer(layers.LayerTypeTCP)
	if tcpLayer == nil {
		return nil
	}
	tcp := tcpLayer.(*layers.TCP)

	return tcp
}

// NewRecord constructs a record to write to the database
func NewRecord(tp *TaggedPacket) (*Record, error) {
	ip4 := unpackIPv4(tp.Packet)
	if ip4 == nil {
		return nil, errors.New("not ip4 type packet")
	}

	udpLayer := tp.Packet.Layer(layers.LayerTypeUDP)
	tcpLayer := tp.Packet.Layer(layers.LayerTypeTCP)
	var (
		srcPort   uint16
		dstPort   uint16
		transport string
	)

	if tcpLayer != nil {
		tcp := tcpLayer.(*layers.TCP)
		srcPort = uint16(tcp.SrcPort)
		dstPort = uint16(tcp.DstPort)
		transport = "tcp"
	} else if udpLayer != nil {
		udp := udpLayer.(*layers.UDP)
		srcPort = uint16(udp.SrcPort)
		dstPort = uint16(udp.DstPort)
		transport = "udp"
	} else {
		return nil, errors.New("nor tcp nor udp type packet")
	}

	var payload []byte
	appLayer := tp.Packet.ApplicationLayer()
	if appLayer != nil {
		payload = appLayer.Payload()
	}

	return &Record{
		SrcIP:          ip4.SrcIP,
		DstIP:          ip4.DstIP,
		SrcPort:        srcPort,
		DstPort:        dstPort,
		Timestamp:      tp.Packet.Metadata().CaptureInfo.Timestamp,
		Payload:        payload,
		PayloadString:  string(payload),
		Tags:           tp.Tags,
		TransportProto: transport,
	}, nil
}

func (b *Badcapt) export(ctx context.Context, tp *TaggedPacket) error {
	record, err := NewRecord(tp)
	if err != nil {
		return err
	}

	if b.client == nil {
		return exportScreen(record)
	}

	return b.exportElastic(ctx, record)
}

func (b *Badcapt) exportElastic(ctx context.Context, record *Record) error {
	_, err := b.client.Index().
		Index(b.indexName).
		Type(b.docType).
		BodyJson(record).
		Do(ctx)

	return err
}

func exportScreen(record *Record) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	fmt.Println(string(data))

	return nil
}

// New bootstraps badcapt configuration.
func New(opts ...func(*Badcapt) error) (*Badcapt, error) {
	conf := &Badcapt{
		client:    nil,
		indexName: indexName,
		docType:   docType,
		markers:   defaultMarkers,
	}

	for _, f := range opts {
		err := f(conf)
		if err != nil {
			return nil, err
		}
	}

	if conf.client == nil {
		return conf, nil
	}

	exists, err := conf.client.IndexExists(indexName).Do(context.Background())
	if err != nil {
		return nil, err
	}

	if !exists {
		_, err := conf.client.CreateIndex(indexName).Do(context.Background())
		if err != nil {
			return nil, err
		}
	}

	return conf, nil
}

// AddPacketMarker adds a packet marking routine.
func AddPacketMarker(m Marker) func(*Badcapt) error {
	return func(b *Badcapt) error {
		b.markers = append(b.markers, m)
		return nil
	}
}

// SetElastic sets elasticsearch client to export events to.
func SetElastic(client *elastic.Client) func(*Badcapt) error {
	return func(b *Badcapt) error {
		b.client = client
		return nil
	}
}

// SetElasticIndexName sets an index name where events are going to be written.
func SetElasticIndexName(name string) func(*Badcapt) error {
	return func(b *Badcapt) error {
		b.indexName = name
		return nil
	}
}

// SetElasticDocType sets the events documents type.
func SetElasticDocType(doc string) func(*Badcapt) error {
	return func(b *Badcapt) error {
		b.docType = doc
		return nil
	}
}

// SetCacheSize to limit it's max size.
func SetCacheSize(size int) func(*Badcapt) error {
	return func(b *Badcapt) error {
		b.cacheSize = size
		return nil
	}
}

// NewConfig bootstraps badcapt configuration.
// Deprecated. Use New instead.
func NewConfig(elasticLoc string, markers ...Marker) (*Badcapt, error) {
	client, err := elastic.NewClient(
		elastic.SetURL(elasticLoc),
		elastic.SetSniff(false),
	)
	if err != nil {
		return nil, err
	}

	conf := &Badcapt{
		client:    client,
		indexName: indexName,
		docType:   docType,
	}

	exists, err := client.IndexExists(indexName).Do(context.Background())
	if err != nil {
		return nil, err
	}

	if !exists {
		_, err := client.CreateIndex(indexName).Do(context.Background())
		if err != nil {
			return nil, err
		}
	}

	if len(markers) == 0 {
		conf.markers = defaultMarkers
	}

	return conf, nil
}

// Listen starts packet sniffing and processing
func (b *Badcapt) Listen(iface string) error {
	handle, err := pcap.OpenLive(iface, 1600, true, pcap.BlockForever)
	if err != nil {
		return err
	}
	defer handle.Close()
	log.Printf("Started capturing on iface %s", iface)

	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	for {
		p, err := packetSource.NextPacket()
		if err == io.EOF {
			break
		} else if err != nil {
			log.Println(err)
			continue
		}

		go func() {
			hErr := b.handle(p)
			if hErr != nil {
				log.Println(hErr)
			}
		}()
	}

	return nil
}

func (b *Badcapt) handle(p gopacket.Packet) error {
	var tags []string

	for _, fn := range b.markers {
		tags = append(tags, fn(p)...)
	}

	for _, sfn := range b.seriesMarkers {
		tags = append(tags, sfn(p)...)
	}

	if len(tags) == 0 {
		return nil
	}

	if err := b.export(context.Background(), &TaggedPacket{p, tags}); err != nil {
		return err
	}

	return nil
}
