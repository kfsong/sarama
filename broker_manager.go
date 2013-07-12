package kafka

import "sync"

type brokerManager struct {
	client        *Client
	defaultBroker *broker

	brokers    map[int32]*broker                       // maps broker ids to brokers
	partitions map[string]map[int32]*partitionMetadata // maps topics to partition ids to partitions
	lock       sync.RWMutex                            // protects access to the maps, only one since they're always accessed together
}

func newBrokerManager(client *Client, host string, port int32) (bm *brokerManager, err error) {
	bm = new(brokerManager)

	bm.client = client

	// we create a new broker object as the default 'master' broker
	// if this broker is also a leader then we will end up with two broker objects for it, but that's not a big deal
	bm.defaultBroker, err = newBroker(host, port)
	if err != nil {
		return nil, err
	}

	bm.brokers = make(map[int32]*broker)
	bm.partitions = make(map[string]map[int32]*partitionMetadata)

	// do an initial fetch of all cluster metadata by specifing an empty list of topics
	err = bm.refreshTopics(make([]*string, 0))
	if err != nil {
		return nil, err
	}

	return bm, nil
}

func (bm *brokerManager) terminateBroker(id int32) {
	bm.lock.Lock()
	delete(bm.brokers, id)
	bm.lock.Unlock()
}

func (bm *brokerManager) getLeader(topic string, partition_id int32) *broker {
	var leader *broker = nil

	bm.lock.RLock()
	defer bm.lock.RUnlock()

	id_map := bm.partitions[topic]
	if id_map != nil {
		partition := id_map[partition_id]
		if partition != nil {
			leader = bm.brokers[partition.leader]
		}
	}

	return leader
}

func (bm *brokerManager) getValidLeader(topic string, partition_id int32) (*broker, error) {

	leader := bm.getLeader(topic, partition_id)

	if leader == nil {
		err := bm.refreshTopic(topic)
		if err != nil {
			return nil, err
		}

		leader = bm.getLeader(topic, partition_id)
	}

	if leader == nil {
		return nil, UNKNOWN_TOPIC_OR_PARTITION
	}

	return leader, nil
}

func (bm *brokerManager) partitionsForTopic(topic string) ([]int32, error) {
	bm.lock.RLock()
	id_map := bm.partitions[topic]
	if id_map == nil {
		bm.lock.RUnlock()
		err := bm.refreshTopic(topic)
		if err != nil {
			return nil, err
		}
		bm.lock.RLock()
		id_map = bm.partitions[topic]
		if id_map == nil {
			bm.lock.RUnlock()
			return nil, UNKNOWN_TOPIC_OR_PARTITION
		}
	}
	partitions := make([]int32, len(id_map))
	i := 0
	for id, _ := range id_map {
		partitions[i] = id
		i++
	}
	bm.lock.RUnlock()
	return partitions, nil
}

func (bm *brokerManager) sendToPartition(topic string, partition int32, req requestEncoder, res responseDecoder) (bool, error) {
	b, err := bm.getValidLeader(topic, partition)
	if err != nil {
		return false, err
	}

	gotResponse, err := b.sendAndReceive(bm.client.id, req, res)
	switch err.(type) {
	case EncodingError:
		// encoding errors are our problem, not the broker's, so just return them
		// rather than refreshing the broker metadata
		return false, err
	case nil:
		// no error, did we get a response?
		if gotResponse {
			// yes, so check for stale topics that may require a resend
			stale := res.staleTopics()
			if len(stale) == 0 {
				return true, nil
			}
			err = bm.refreshTopics(stale)
			if err != nil {
				return true, err
			}
		} else {
			// no, so we have to assume it worked
			return false, nil
		}
	default:
		// broker error, so discard that broker
		bm.terminateBroker(b.id)
	}

	// then do the whole thing again
	// (the metadata for the broker gets refreshed automatically in getValidLeader)
	// if we get a broker here, it's guaranteed to be fresh, so if it fails then
	// we pass that error back to the user (as opposed to retrying indefinitely)
	b, err = bm.getValidLeader(topic, partition)
	if err != nil {
		return false, err
	}

	return b.sendAndReceive(bm.client.id, req, res)
}

func (bm *brokerManager) getDefault() *broker {

	if bm.defaultBroker == nil {
		bm.lock.RLock()
		defer bm.lock.RUnlock()
		for id, _ := range bm.brokers {
			bm.defaultBroker = bm.brokers[id]
			break
		}
	}

	return bm.defaultBroker
}

func (bm *brokerManager) sendToAny(req requestEncoder, res decoder) (bool, error) {
	for b := bm.getDefault(); b != nil; b = bm.getDefault() {
		gotResponse, err := b.sendAndReceive(bm.client.id, req, res)
		switch err.(type) {
		case nil, EncodingError:
			return gotResponse, err
		default:
			// broker error, so discard that broker
			bm.defaultBroker = nil
			bm.terminateBroker(b.id)
		}
	}
	return false, OutOfBrokers{}
}

func (bm *brokerManager) refreshTopics(topics []*string) error {
	response := new(metadata)
	_, err := bm.sendToAny(&metadataRequest{topics}, response)
	if err != nil {
		return err
	}

	bm.lock.Lock()
	defer bm.lock.Unlock()

	for i := range response.brokers {
		broker := &response.brokers[i]
		bm.brokers[broker.id] = broker
	}

	for i := range response.topics {
		topic := &response.topics[i]
		if topic.err != NO_ERROR {
			return topic.err
		}
		bm.partitions[*topic.name] = make(map[int32]*partitionMetadata, len(topic.partitions))
		for j := range topic.partitions {
			partition := &topic.partitions[j]
			if partition.err != NO_ERROR {
				return partition.err
			}
			bm.partitions[*topic.name][partition.id] = partition
		}
	}

	return nil
}

func (bm *brokerManager) refreshTopic(topic string) error {
	tmp := make([]*string, 1)
	tmp[0] = &topic
	return bm.refreshTopics(tmp)
}