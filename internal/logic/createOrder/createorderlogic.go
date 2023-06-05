package createOrder

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/firstsatoshi/website/common/uniqueid"
	"github.com/firstsatoshi/website/internal/svc"
	"github.com/firstsatoshi/website/internal/types"
	"github.com/firstsatoshi/website/model"
	"github.com/firstsatoshi/website/xerr"
	"github.com/pkg/errors"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateOrderLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewCreateOrderLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateOrderLogic {
	return &CreateOrderLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func calcFee(count, feeRate float64) int64 {
	// 每个铭文固定金额
	utxoSat := float64(10000)
	averageFileSize := float64(2600) // 是 2600byte 不是 2600Byte

	utxoOutputValue := float64(utxoSat) * count
	commitTxSize := 68 + (43+1)*count
	commitTxSize += 64
	revealTxSize := 10.5 + (57.5+43.0)*float64(count)
	revealTxSize += 64
	feeSats := math.Ceil((averageFileSize/4 + commitTxSize + revealTxSize) * feeRate)
	feeSats = 1000 * math.Ceil(feeSats/1000)

	// base fee
	baseService := 1000 * math.Ceil(feeRate*0.1/1000)
	feeSats += baseService

	total := feeSats + utxoOutputValue
	return int64(total)
}

func (l *CreateOrderLogic) CreateOrder(req *types.CreateOrderReq) (resp *types.CreateOrderResp, err error) {

	// check count
	if req.Count <= 0 {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.REUQEST_PARAM_ERROR), "count is invalid %v", req.Count)
	}
	if req.Count > 10 {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.COUNT_EXCEED_PER_ORDER_LIMIT_ERROR), "count is too large %v", req.Count)
	}

	// check receiveAddress is valid P2TR address
	_, err = btcutil.DecodeAddress(req.ReceiveAddress, l.svcCtx.ChainCfg)
	if err != nil {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.INVALID_BTCP2TRADDRESS_ERROR), "invalid receive address %v", req.ReceiveAddress)
	}

	// TODO get mempool recommanded feerate
	// https://mempool.space/api/v1/fees/recommended
	if l.svcCtx.ChainCfg.Name == chaincfg.MainNetParams.Name && (req.FeeRate < 10 || req.FeeRate > 200) {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.FEERATE_TOO_SMALL_ERROR), "feeRate too small %v", req.FeeRate)
	}

	event, err := l.svcCtx.TbBlindboxEventModel.FindOne(l.ctx, int64(req.EventId))
	if err != nil {
		if err == model.ErrNotFound {
			return nil, errors.Wrapf(xerr.NewErrCode(xerr.EVENT_NOT_EXISTS_ERROR), "event id does not exists %v", req.EventId)
		} else {
			logx.Errorf("FindOne error:%v", err.Error())
			return nil, errors.Wrapf(xerr.NewErrCode(xerr.SERVER_COMMON_ERROR), "FindOne error: %v", err.Error())
		}
	}

	// check available count
	if event.Avail < int64(req.Count) {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.AVAILABLE_COUNT_IS_NOT_ENOUGH),
			"avail count %v is not enough %v", event.Avail, req.Count)
	}

	// query exists unpayment order
	// queryBuilder := l.svcCtx.TbOrderModel.RowBuilder().Where(squirrel.Eq{
	// 	"receive_address": req.ReceiveAddress,
	// 	"order_status": "NOTPAID",
	// }).OrderBy("id ASC")
	// l.svcCtx.TbOrderModel.FindOrders(l.ctx, queryBuilder)

	// random generate account_index and address_index
	time.Sleep(time.Microsecond * 1)
	rand.Seed(time.Now().UnixNano() + int64(req.Count) + int64(req.FeeRate) + int64(req.ReceiveAddress[10]) + int64(req.ReceiveAddress[17]))

	accountIndex := rand.Uint32()
	addressIndex := rand.Uint32()
	for {
		_, e := l.svcCtx.TbAddressModel.FindOneByCoinTypeAccountIndexAddressIndex(l.ctx, "BTC", int64(accountIndex), int64(addressIndex))
		if e == model.ErrNotFound {
			break
		}

		// if already exists continue generate random index
		accountIndex = rand.Uint32()
		addressIndex = rand.Uint32()
	}

	_, depositAddress, err := l.svcCtx.KeyManager.GetWifKeyAndAddresss(
		accountIndex,
		addressIndex)
	if err != nil {
		logx.Errorf("error: %v", err.Error())
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.SERVER_COMMON_ERROR), "generate address error %v", err.Error())
	}

	addresInsertResult, err := l.svcCtx.TbAddressModel.Insert(l.ctx, &model.TbAddress{
		Address:      depositAddress,
		CoinType:     "BTC",
		AccountIndex: int64(accountIndex),
		AddressIndex: int64(addressIndex),
	})
	if err != nil {
		logx.Errorf("error: %v", err.Error())
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.SERVER_COMMON_ERROR), "insert address error %v", err.Error())
	}
	addressId, err := addresInsertResult.LastInsertId()
	if err != nil {
		addressId = 0
	}

	prefix := "BX" + req.ReceiveAddress[4:8] + req.ReceiveAddress[len(req.ReceiveAddress)-4:] +
		depositAddress[4:8] + depositAddress[len(depositAddress)-4:] +
		fmt.Sprintf("%02d", req.Count) + fmt.Sprintf("%02d", req.FeeRate)
	prefix = strings.ToUpper(prefix)
	orderId := uniqueid.GenSn(prefix)

	// https://bitcoinops.org/en/tools/calc-size/
	// https://blockchain-academy.hs-mittweida.de/2023/02/calculation-of-bitcoin-transaction-fees-explained/
	// for p2tr:
	//  Transaction size = Overhead +  57.5 * inputsNumber + 43 * outputsNumber
	//      eg: 1 input 1 output: 10.5 + 57.5 * 1 + 43 * 1 = 111 bytes

	totalFee := calcFee(float64(req.Count), float64(req.FeeRate))
	if totalFee+event.PriceSats < event.PriceSats {
		panic("invalid price or totalFee")
	}

	createTime := time.Now()
	ord := model.TbOrder{
		OrderId:         orderId,
		EventId:         int64(req.EventId),
		Count:           int64(req.Count),
		DepositAddress:  depositAddress,
		ReceiveAddress:  req.ReceiveAddress,
		InscriptionData: "BitEagle Blindbox", // no use
		FeeRate:         int64(req.FeeRate),
		TxfeeAmountSat:  totalFee,
		ServiceFeeSat:   0,
		PriceSat:        event.PriceSats, // 0.002
		TotalAmountSat:  totalFee + event.PriceSats,
		OrderStatus:     "NOTPAID",
		Version:         0,
		CreateTime:      createTime,
	}
	_, err = l.svcCtx.TbOrderModel.Insert(l.ctx, &ord)
	if err != nil {
		// TODO: use transaction ?
		l.svcCtx.TbAddressModel.Delete(l.ctx, addressId)

		logx.Errorf("insert error:%v", err.Error())
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.SERVER_COMMON_ERROR), "insert error: %v", err.Error())
	}

	// update bloomFilter for deposit
	if err = l.svcCtx.DepositBloomFilter.Add([]byte(depositAddress)); err != nil {
		time.Sleep(time.Millisecond * 17)
		l.svcCtx.DepositBloomFilter.Add([]byte(depositAddress))
	}

	resp = &types.CreateOrderResp{
		OrderId:        ord.OrderId,
		EventId:        int(ord.EventId),
		Count:          int(ord.Count),
		DepositAddress: ord.DepositAddress,
		ReceiveAddress: ord.ReceiveAddress,
		FeeRate:        int(ord.FeeRate),
		Bytes:          0,
		InscribeFee:    int(ord.TxfeeAmountSat),
		ServiceFee:     int(ord.ServiceFeeSat),
		Price:          int(ord.PriceSat),
		Total:          int(ord.TotalAmountSat),
		// CreateTime:     time.Now().Format("2006-01-02 15:04:05"),
		CreateTime: createTime.Format("2006-01-02 15:04:05 +0800 CST"),
	}

	return
}
