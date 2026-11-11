package mongodb

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestInsightsGroupByQueryHash(t *testing.T) {
	engine := Engine{admin: fakeAdmin{replies: map[string]bson.M{
		"listDatabases": {"databases": bson.A{bson.M{"name": "shop"}, bson.M{"name": "admin"}}},
		"aggregate": {"cursor": bson.M{"firstBatch": bson.A{
			bson.M{"_id": "A1B2", "calls": 4, "totalMs": 10, "ns": "shop.orders", "op": "query", "filter": bson.D{{Key: "status", Value: "paid"}, {Key: "total", Value: bson.D{{Key: "$gt", Value: 100}}}}},
			bson.M{"_id": "shop.orders insert", "calls": 2, "totalMs": 30, "ns": "shop.orders", "op": "insert"},
		}}},
	}}}

	insights, err := engine.Nodes().Insights(context.Background())

	if err != nil || len(insights.Statements) != 2 {
		t.Fatalf("insights = %+v, %v", insights, err)
	}
	first, second := insights.Statements[0], insights.Statements[1]
	if first.Text != "shop.orders insert" || first.TotalMs != 30 || first.MeanMs != 15 {
		t.Errorf("first = %+v", first)
	}
	if second.Text != `shop.orders query {"status":"?","total":{"$gt":"?"}}` || second.Calls != 4 || second.MeanMs != 2 {
		t.Errorf("second = %+v", second)
	}
}
