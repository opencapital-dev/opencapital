-- ============================================================================
-- fold_kernel — v5 per-event portfolio fold UDAF.
-- ----------------------------------------------------------------------------
-- Ports common/portfolio_engine + common/finance into a single embedded
-- Python UDAF. Maintains:
--   * Per-instrument FIFO + AvgCost accumulators (cost_basis.py).
--   * Per-currency CashBucket with FIFO + borrow lots (cash_bucket.py).
--   * PendingBorrow registry for margin loans (cash_replay.py).
--   * Per-instrument open-lot tracker → foreign-sell FX basis.
--   * Per-currency realized counters: interest, dividends, fees.
--   * Per-instrument round-trip cycle state.
--
-- Returns STRUCT<snapshot JSONB, closures JSONB, cycles JSONB, dirty BOOLEAN>.
--   * snapshot.equity_positions: one entry per currently held instrument.
--   * snapshot.cash_positions: one entry per currency.
--   * snapshot.portfolio_core: aggregate rollups (realized_pnl, fees,
--     dividends, interest).
--   * closures: lot closures emitted by THIS event (per-event side stream).
--   * cycles: cycles closed by THIS event.
--   * dirty: true if a retract diverged.
--
-- Event types: TRADE, CASHFLOW (DEPOSIT/WITHDRAWAL/INTEREST_ON_CASH/FEE),
-- DIVIDEND, TRANSFER_IN, FX_CONVERSION.
-- ============================================================================

DROP AGGREGATE IF EXISTS fold_kernel(JSONB);

CREATE AGGREGATE fold_kernel(event JSONB)
RETURNS STRUCT<snapshot JSONB, closures JSONB, cycles JSONB, dirty BOOLEAN>
LANGUAGE python AS $$

import copy
import datetime

EPS = 1e-9
EPS_LOT = 1e-12


# ============================================================================
# Helpers
# ============================================================================

def _to_float(v, default=0.0):
    if v is None:
        return default
    try:
        return float(v)
    except (TypeError, ValueError):
        return default


def _us_to_iso(us):
    if us is None:
        return None
    return datetime.datetime.fromtimestamp(us / 1_000_000, tz=datetime.timezone.utc).isoformat()


def _add_base(current, amount, fx_at):
    """Accumulate base currency amount; propagate None when fx missing."""
    if current is None:
        return None
    if amount == 0.0:
        return current
    if fx_at is None:
        return None
    return current + amount * fx_at


def _clone_state(s):
    # RW pickles state across barriers; we deepcopy inside accumulate so
    # the input dict is treated as immutable per the streaming contract.
    return copy.deepcopy(s)


# ============================================================================
# Cost-basis accumulators (FIFO + AvgCost)
# ============================================================================

def _new_fifo():
    return {
        'direction': 'flat',
        'qty': 0.0,
        'lots': [],
        'realized_gross_native': 0.0,
        'realized_gross_base': 0.0,
        'realized_fx_gain_base': 0.0,
    }


def _new_avg():
    return {
        'direction': 'flat',
        'qty': 0.0,
        'avg_native': 0.0,
        'avg_base': 0.0,
        'avg_fx_at_buy': 0.0,
        'avg_multiplier': 1.0,
        'realized_gross_native': 0.0,
        'realized_gross_base': 0.0,
        'realized_fx_gain_base': 0.0,
    }


def _fifo_open(fifo, lot, direction):
    if fifo['direction'] == 'flat':
        fifo['direction'] = direction
    elif fifo['direction'] != direction:
        # Programming error — caller must close first.
        return
    fifo['lots'].append(lot)
    fifo['qty'] += lot['quantity']


def _fifo_close(fifo, qty, close_price, close_price_base, close_fx,
                realize_fx, exit_ts_us, instrument_id, out_closures):
    """Returns realized base PnL contribution for this close (sum across lots)."""
    if fifo['direction'] == 'flat' or qty <= EPS_LOT:
        return 0.0
    remaining = qty
    sign = 1.0 if fifo['direction'] == 'long' else -1.0
    direction = fifo['direction']
    realized_base_delta = 0.0
    new_lots = []
    for lot in fifo['lots']:
        if remaining <= EPS_LOT:
            new_lots.append(lot)
            continue
        take = lot['quantity'] if lot['quantity'] <= remaining else remaining
        mult = lot.get('contract_multiplier', 1.0) or 1.0
        pnl_native = take * (close_price - lot['price']) * sign * mult
        if realize_fx:
            pnl_base = take * (close_price_base - lot['price_base']) * sign * mult
            fx_gain = take * lot['price'] * (close_fx - lot['fx_at_buy']) * sign * mult
        else:
            pnl_base = take * (close_price - lot['price']) * lot['fx_at_buy'] * sign * mult
            fx_gain = 0.0
        fifo['realized_gross_native'] += pnl_native
        fifo['realized_gross_base']   += pnl_base
        fifo['realized_fx_gain_base'] += fx_gain
        realized_base_delta += pnl_base

        out_closures.append({
            'lot_id':           lot['lot_id'],
            'direction':        direction,
            'instrument_id':    instrument_id,
            'entry_ts':         _us_to_iso(lot['event_ts_us']),
            'exit_ts':          _us_to_iso(exit_ts_us),
            'qty':              take,
            'entry_price':      lot['price'],
            'entry_price_base': lot['price_base'],
            'entry_fx_to_base': lot['fx_at_buy'],
            'exit_price':       close_price,
            'exit_price_base':  close_price_base,
            'exit_fx_to_base':  close_fx,
            'realized_pnl_native': pnl_native,
            'realized_pnl_base':   pnl_base,
        })

        # Scale lot's commission/fees down by the fraction consumed.
        if lot['quantity'] > EPS_LOT:
            share = take / lot['quantity']
            lot['commission']      -= lot['commission']      * share
            lot['commission_base'] -= lot['commission_base'] * share
            lot['fees']            -= lot['fees']            * share
            lot['fees_base']       -= lot['fees_base']       * share
        lot['quantity'] -= take
        remaining       -= take
        if lot['quantity'] > EPS_LOT:
            new_lots.append(lot)
    fifo['lots'] = new_lots
    fifo['qty'] = max(0.0, fifo['qty'] - qty)
    if fifo['qty'] <= EPS_LOT:
        fifo['qty'] = 0.0
        fifo['direction'] = 'flat'
        fifo['lots'] = []
    return realized_base_delta


def _avg_open(avg, lot):
    if avg['direction'] == 'flat':
        avg['direction'] = 'long'  # Same direction inferred via FIFO
    new_qty = avg['qty'] + lot['quantity']
    if new_qty <= EPS_LOT:
        avg['avg_native']     = lot['price']
        avg['avg_base']       = lot['price_base']
        avg['avg_fx_at_buy']  = lot['fx_at_buy']
        avg['avg_multiplier'] = lot.get('contract_multiplier', 1.0) or 1.0
    else:
        old_qty = avg['qty']
        new_q   = lot['quantity']
        avg['avg_native']     = (old_qty * avg['avg_native']     + new_q * lot['price'])         / new_qty
        avg['avg_base']       = (old_qty * avg['avg_base']       + new_q * lot['price_base'])    / new_qty
        avg['avg_fx_at_buy']  = (old_qty * avg['avg_fx_at_buy']  + new_q * lot['fx_at_buy'])     / new_qty
        avg['avg_multiplier'] = (old_qty * avg['avg_multiplier'] + new_q * (lot.get('contract_multiplier', 1.0) or 1.0)) / new_qty
    avg['qty'] = new_qty


def _avg_close(avg, qty, close_price, close_price_base, close_fx, realize_fx):
    if avg['direction'] == 'flat' or qty <= EPS_LOT:
        return
    sign = 1.0 if avg['direction'] == 'long' else -1.0
    mult = avg['avg_multiplier']
    avg['realized_gross_native'] += qty * (close_price - avg['avg_native']) * sign * mult
    if realize_fx:
        avg['realized_gross_base']   += qty * (close_price_base - avg['avg_base'])    * sign * mult
        avg['realized_fx_gain_base'] += qty * avg['avg_native'] * (close_fx - avg['avg_fx_at_buy']) * sign * mult
    else:
        avg['realized_gross_base']   += qty * (close_price - avg['avg_native']) * avg['avg_fx_at_buy'] * sign * mult
    avg['qty'] -= qty
    if avg['qty'] <= EPS_LOT:
        avg['qty'] = 0.0
        avg['direction'] = 'flat'
        avg['avg_native']     = 0.0
        avg['avg_base']       = 0.0
        avg['avg_fx_at_buy']  = 0.0
        avg['avg_multiplier'] = 1.0


# ============================================================================
# CashBucket
# ============================================================================

def _new_bucket():
    return {
        'fifo_lots': [],   # [{lot_id, event_ts_us, quantity, fx_to_base}]
        'borrow_lots': [], # [{borrow_id, event_ts_us, quantity, consumer, consumer_kind}]
        'avg_fx_to_base': 0.0,
        'lot_qty': 0.0,
        'realized_fx_gain_fifo_base': 0.0,
        'realized_fx_gain_avg_base': 0.0,
        'borrow_seq': 0,
    }


def _bucket_deposit(state, ccy, qty, fx_to_base, lot_id, event_ts_us):
    bucket = state['currencies'][ccy]['bucket']
    if qty <= EPS:
        return
    remainder = qty
    # FIFO-cover open borrows first.
    while remainder > EPS and bucket['borrow_lots']:
        borrow = bucket['borrow_lots'][0]
        take = borrow['quantity'] if borrow['quantity'] <= remainder else remainder
        borrow['quantity'] -= take
        bucket['lot_qty']   += take
        remainder           -= take
        _on_cover(state, borrow['borrow_id'], borrow['consumer'],
                  borrow['consumer_kind'], take, fx_to_base, event_ts_us)
        if borrow['quantity'] <= EPS:
            bucket['borrow_lots'].pop(0)
    if -EPS < bucket['lot_qty'] < EPS:
        bucket['lot_qty'] = 0.0
    if remainder <= EPS:
        return
    bucket['fifo_lots'].append({
        'lot_id':       lot_id,
        'event_ts_us':  event_ts_us,
        'quantity':     remainder,
        'fx_to_base':   fx_to_base,
    })
    positive_before = bucket['lot_qty'] if bucket['lot_qty'] > EPS else 0.0
    new_total = positive_before + remainder
    bucket['avg_fx_to_base'] = (
        positive_before * bucket['avg_fx_to_base'] + remainder * fx_to_base
    ) / new_total
    bucket['lot_qty'] = bucket['lot_qty'] + remainder


def _bucket_drain(state, ccy, amount, broker_fx_to_base, consumer,
                  consumer_kind, event_ts_us):
    """Returns DrainResult dict {lot_backed_basis, lot_backed_qty, borrowed_qty, borrow_id}."""
    bucket = state['currencies'][ccy]['bucket']
    if amount <= EPS:
        return {'lot_backed_basis': None, 'lot_backed_qty': 0.0,
                'borrowed_qty': 0.0, 'borrow_id': None}
    lot_backed = min(amount, max(bucket['lot_qty'], 0.0))
    # AvgCost FX realization on draining lot-backed portion (only when this
    # is a convert, ie broker_fx_to_base is not None).
    if broker_fx_to_base is not None and lot_backed > EPS:
        bucket['realized_fx_gain_avg_base'] += lot_backed * (
            broker_fx_to_base - bucket['avg_fx_to_base']
        )
    remaining       = amount
    drained_qty     = 0.0
    drained_basis_sum = 0.0
    new_fifo = []
    for lot in bucket['fifo_lots']:
        if remaining <= EPS:
            new_fifo.append(lot)
            continue
        take = lot['quantity'] if lot['quantity'] <= remaining else remaining
        if broker_fx_to_base is not None:
            bucket['realized_fx_gain_fifo_base'] += take * (
                broker_fx_to_base - lot['fx_to_base']
            )
        drained_qty       += take
        drained_basis_sum += take * lot['fx_to_base']
        lot['quantity']   -= take
        bucket['lot_qty'] -= take
        remaining         -= take
        if lot['quantity'] > EPS:
            new_fifo.append(lot)
    bucket['fifo_lots'] = new_fifo
    if -EPS < bucket['lot_qty'] < EPS and not bucket['borrow_lots']:
        bucket['lot_qty'] = 0.0
        bucket['avg_fx_to_base'] = 0.0

    borrow_id = None
    if remaining > EPS:
        bucket['borrow_seq'] += 1
        borrow_id = (consumer or 'borrow') + '#' + str(bucket['borrow_seq'])
        bucket['borrow_lots'].append({
            'borrow_id':      borrow_id,
            'event_ts_us':    event_ts_us,
            'quantity':       remaining,
            'consumer':       consumer,
            'consumer_kind':  consumer_kind,
        })
        bucket['lot_qty']        -= remaining
        bucket['avg_fx_to_base']  = 0.0
    return {
        'lot_backed_basis': (drained_basis_sum / drained_qty
                             if drained_qty > EPS else None),
        'lot_backed_qty':   drained_qty,
        'borrowed_qty':     remaining if remaining > EPS else 0.0,
        'borrow_id':        borrow_id,
    }


# ============================================================================
# PendingBorrow lifecycle
# ============================================================================

def _on_cover(state, borrow_id, consumer, consumer_kind, covered_qty,
              cover_fx_to_base, cover_ts_us):
    pb = state['pending_borrows'].get(borrow_id)
    if pb is None or pb.get('finalized'):
        return
    pb['cover_slices'].append([covered_qty, cover_fx_to_base])
    pb['covered_qty'] += covered_qty
    if pb['covered_qty'] >= pb['borrowed_qty'] - EPS:
        _finalize_borrow(state, pb)


def _finalize_borrow(state, pb, fallback_fx=None):
    if pb.get('finalized'):
        return
    slices   = list(pb['cover_slices'])
    residual = pb['borrowed_qty'] - pb['covered_qty']
    if residual > EPS:
        rate = fallback_fx if fallback_fx is not None else 1.0
        slices.append([residual, rate])
    cover_qty      = sum(q for q, _ in slices)
    cover_weighted = sum(q * fx for q, fx in slices)
    cover_rate     = cover_weighted / cover_qty if cover_qty > EPS else 1.0

    kind = pb['consumer_kind']
    if kind == 'buy':
        tot = pb['lot_backed_qty'] + cover_qty
        num = (pb['lot_backed_qty'] * (pb['lot_backed_basis']
               if pb['lot_backed_basis'] is not None else cover_rate)
               + cover_weighted)
        basis = num / tot if tot > EPS else cover_rate
        state['buy_fx_basis'][pb['consumer']] = basis
        if pb.get('fee_native'):
            src = state['currencies'].get(pb['from_ccy'])
            if src is not None:
                src['fees_native'] += pb['fee_native']
                src['fees_base']    = _add_base(src['fees_base'],
                                                pb['fee_native'], basis)
    elif kind == 'convert':
        src = state['currencies'].get(pb['from_ccy'])
        if src is not None:
            gain = pb['borrowed_qty'] * pb['from_broker_fx'] - cover_weighted
            src['bucket']['realized_fx_gain_fifo_base'] += gain
            src['bucket']['realized_fx_gain_avg_base']  += gain
    elif kind == 'fee':
        src = state['currencies'].get(pb['from_ccy'])
        if src is not None and src['fees_base'] is not None:
            src['fees_base'] += pb['borrowed_qty'] * cover_rate
    pb['finalized'] = True


# ============================================================================
# Foreign-sell FX basis tracker
# ============================================================================

def _tracker_buy(state, instrument_id, qty, trade_id):
    state['open_lots_fx_tracker'].setdefault(instrument_id, []).append([qty, trade_id])


def _tracker_sell(state, instrument_id, qty, fallback_fx):
    lots = state['open_lots_fx_tracker'].get(instrument_id, [])
    total = sum(l[0] for l in lots)
    if total <= EPS:
        return fallback_fx
    weighted = sum(l[0] * state['buy_fx_basis'].get(l[1], fallback_fx) for l in lots)
    avg = weighted / total
    sold = qty if qty <= total else total
    scale = (total - sold) / total
    new_lots = []
    for l in lots:
        l[0] *= scale
        if l[0] > EPS:
            new_lots.append(l)
    state['open_lots_fx_tracker'][instrument_id] = new_lots
    return avg


# ============================================================================
# Per-instrument state + dispatch
# ============================================================================

def _new_inst(currency, kind):
    return {
        'currency':         currency,
        'instrument_kind':  kind,
        'n_trades':         0,
        'fifo':             _new_fifo(),
        'avg':              _new_avg(),
        'cycle_count':      0,
        'cycle_open_ts_us': None,
        'cycle_pnl':        0.0,
    }


def _new_ccy():
    return {
        'bucket':                    _new_bucket(),
        'realized_interest_native':  0.0,
        'realized_interest_base':    0.0,
        'realized_dividends_native': 0.0,
        'realized_dividends_base':   0.0,
        'fees_native':               0.0,
        'fees_base':                 0.0,
        'deposits':                  0.0,
        'withdrawals':               0.0,
        'interest':                  0.0,
        'fees_cum':                  0.0,
    }


def _get_inst(state, iid, currency, kind):
    if iid not in state['instruments']:
        state['instruments'][iid] = _new_inst(currency, kind)
    return state['instruments'][iid]


def _get_ccy(state, ccy):
    if ccy not in state['currencies']:
        state['currencies'][ccy] = _new_ccy()
    return state['currencies'][ccy]


# ---------- TRADE ----------

def _apply_trade(state, ev, out_closures, out_cycles):
    iid = ev.get('instrument_id')
    if not iid:
        return
    qty = _to_float(ev.get('quantity'))
    if qty <= 0:
        return
    side = ev.get('side')
    if side not in ('buy', 'sell'):
        return
    base = state['base_currency']
    ccy = (ev.get('currency') or base).upper()
    kind_meta = ev.get('instrument_kind') or 'equity'
    inst = _get_inst(state, iid, ccy, kind_meta)
    _get_ccy(state, ccy)

    price       = _to_float(ev.get('price'))
    fx_rate_t   = ev.get('fx_rate_to_base')
    fx_rate_t   = float(fx_rate_t) if fx_rate_t is not None else None
    is_base     = ccy == base
    is_foreign  = (not is_base)
    realize_fx  = (fx_rate_t is not None) or not is_foreign
    commission  = _to_float(ev.get('commission'))
    fees        = _to_float(ev.get('fees'))
    fees_total  = commission + fees
    fx_fees_native = _to_float(ev.get('fx_fees_native'))
    mult        = _to_float(ev.get('contract_multiplier'), 1.0) or 1.0
    gross_native= qty * price
    ts_us       = ev.get('business_ts')
    trade_id    = ev.get('source_id')

    inst['n_trades'] += 1

    # ------ Trade settled in base currency (broker auto-converted) ------
    settled_in_base = (fx_rate_t is not None) and is_foreign
    if settled_in_base:
        rate = fx_rate_t
        base_ccy = _get_ccy(state, base)
        if side == 'buy':
            _bucket_drain(state, base, (gross_native + fees_total) * rate,
                          None, trade_id, 'fee', ts_us)
        else:
            _bucket_deposit(state, base, (gross_native - fees_total) * rate,
                            1.0, trade_id, ts_us)
        # Fee accounting in base currency.
        fee_base = fees_total * rate
        base_ccy['fees_native'] += fee_base
        base_ccy['fees_base']    = _add_base(base_ccy['fees_base'], fee_base, 1.0)
        if fx_fees_native:
            fx_fees_ccy = (ev.get('fx_fees_currency') or ev.get('currency') or '').upper()
            fx_fees_fx  = rate if fx_fees_ccy == ccy else 1.0
            if fx_fees_ccy == base:
                base_ccy['fees_native'] += fx_fees_native
            base_ccy['fees_base']    = _add_base(base_ccy['fees_base'],
                                                 fx_fees_native, fx_fees_fx)
        # Set buy_fx_basis from the provided rate for foreign buys (no borrow needed).
        if side == 'buy':
            state['buy_fx_basis'][trade_id] = rate
            _tracker_buy(state, iid, qty, trade_id)
        else:
            avg_fx = _tracker_sell(state, iid, qty, rate)
            state['foreign_sell_fx_basis'][trade_id] = avg_fx
    else:
        # ------ Cash leg in trade currency ------
        ccy_st = _get_ccy(state, ccy)
        if side == 'buy':
            settle = gross_native + fees_total
            res = _bucket_drain(state, ccy, settle, None, trade_id, 'buy', ts_us)
            fee_fx = None
            if not is_base:
                # Provisional buy_fx_basis from lot-backed portion, or broker rate.
                prov = res['lot_backed_basis'] if res['lot_backed_basis'] is not None else 1.0
                state['buy_fx_basis'][trade_id] = prov
                if res['borrowed_qty'] > EPS and res['borrow_id'] is not None:
                    state['pending_borrows'][res['borrow_id']] = {
                        'consumer':         trade_id,
                        'consumer_kind':    'buy',
                        'from_ccy':         ccy,
                        'borrowed_qty':     res['borrowed_qty'],
                        'lot_backed_qty':   res['lot_backed_qty'],
                        'lot_backed_basis': res['lot_backed_basis'],
                        'cover_slices':     [],
                        'covered_qty':      0.0,
                        'finalized':        False,
                        'fee_native':       fees_total,
                        'from_broker_fx':   1.0,
                    }
                    fee_fx = None  # fees recognised on cover
                else:
                    fee_fx = prov
                _tracker_buy(state, iid, qty, trade_id)
            else:
                fee_fx = 1.0
            if fee_fx is not None:
                ccy_st['fees_native'] += fees_total
                ccy_st['fees_base']    = _add_base(ccy_st['fees_base'], fees_total, fee_fx)
            if fx_fees_native:
                fx_fees_ccy = (ev.get('fx_fees_currency') or ev.get('currency') or '').upper()
                if fx_fees_ccy == ccy:
                    ccy_st['fees_native'] += fx_fees_native
                    ccy_st['fees_base']    = _add_base(ccy_st['fees_base'], fx_fees_native, 1.0)
        else:
            proceeds = gross_native - fees_total
            fee_fx = 1.0
            if not is_base:
                avg_fx = _tracker_sell(state, iid, qty, 1.0)
                state['foreign_sell_fx_basis'][trade_id] = avg_fx
                _bucket_deposit(state, ccy, proceeds, avg_fx, trade_id, ts_us)
                fee_fx = avg_fx
            else:
                _bucket_deposit(state, ccy, proceeds, 1.0, trade_id, ts_us)
            if fee_fx is not None:
                ccy_st['fees_native'] += fees_total
                ccy_st['fees_base']    = _add_base(ccy_st['fees_base'], fees_total, fee_fx)

    # ------ Equity side (FIFO + AvgCost) ------
    # fx_at_buy comes from either: trade.fx_rate_to_base, OR provisional
    # buy_fx_basis set above (for cash-side foreign buy), OR 1.0 fallback.
    if side == 'buy':
        fx_at_buy = fx_rate_t if fx_rate_t is not None else (
            state['buy_fx_basis'].get(trade_id, 1.0)
        )
        lot = {
            'lot_id':              trade_id,
            'event_ts_us':         ts_us,
            'quantity':            qty,
            'price':               price,
            'price_base':          price * fx_at_buy,
            'fx_at_buy':           fx_at_buy,
            'commission':          commission,
            'commission_base':     commission * fx_at_buy,
            'fees':                fees,
            'fees_base':           fees * fx_at_buy,
            'contract_multiplier': mult,
        }
        was_flat = inst['fifo']['direction'] == 'flat'
        _fifo_open(inst['fifo'], lot, 'long')
        # Avg uses an independent clone (no shared mutation).
        _avg_open(inst['avg'], dict(lot))
        if was_flat:
            inst['cycle_open_ts_us'] = ts_us
            inst['cycle_pnl']        = 0.0
    else:
        # Sell direction (close).
        fx_at_sell = fx_rate_t if fx_rate_t is not None else (
            state.get('foreign_sell_fx_basis', {}).get(trade_id, 1.0)
        )
        close_price_base = price * fx_at_sell
        # FIFO close.
        realized_delta = _fifo_close(
            inst['fifo'], qty, price, close_price_base, fx_at_sell,
            realize_fx, ts_us, iid, out_closures,
        )
        inst['cycle_pnl'] += realized_delta
        # Avg close.
        _avg_close(inst['avg'], qty, price, close_price_base, fx_at_sell, realize_fx)
        # Cycle close detection.
        if inst['fifo']['direction'] == 'flat' and inst['cycle_open_ts_us'] is not None:
            open_ts_us   = inst['cycle_open_ts_us']
            duration_sec = (ts_us - open_ts_us) / 1_000_000
            out_cycles.append({
                'cycle_seq':         inst['cycle_count'],
                'open_ts':           _us_to_iso(open_ts_us),
                'close_ts':          _us_to_iso(ts_us),
                'pnl_base':          inst['cycle_pnl'],
                'duration_sec':      duration_sec,
                'closing_event_id':  trade_id,
            })
            inst['cycle_count']      += 1
            inst['cycle_open_ts_us']  = None
            inst['cycle_pnl']         = 0.0


# ---------- CASHFLOW ----------

def _apply_cashflow(state, ev, out_closures, out_cycles):
    amount = _to_float(ev.get('amount_native'))
    if amount == 0:
        return
    base = state['base_currency']
    ccy = (ev.get('currency') or base).upper()
    is_base = ccy == base
    fx_at = float(ev.get('event_time_fx')) if ev.get('event_time_fx') is not None else (
        1.0 if is_base else None
    )
    if fx_at is None:
        fx_at = 1.0
    fx_for_bucket = fx_at if fx_at is not None else 1.0
    kind = (ev.get('type') or '').upper()
    ts_us = ev.get('business_ts')
    sid = ev.get('source_id')
    ccy_st = _get_ccy(state, ccy)
    if kind == 'DEPOSIT':
        _bucket_deposit(state, ccy, amount, fx_for_bucket, sid, ts_us)
        ccy_st['deposits'] += amount
    elif kind == 'INTEREST_ON_CASH':
        _bucket_deposit(state, ccy, amount, fx_for_bucket, sid, ts_us)
        ccy_st['interest']                 += amount
        ccy_st['realized_interest_native'] += amount
        ccy_st['realized_interest_base']    = _add_base(
            ccy_st['realized_interest_base'], amount, fx_at,
        )
    elif kind == 'WITHDRAWAL':
        _bucket_drain(state, ccy, amount, None, sid, 'fee', ts_us)
        ccy_st['withdrawals'] += amount
    elif kind == 'FEE':
        _bucket_drain(state, ccy, amount, None, sid, 'fee', ts_us)
        ccy_st['fees_cum']    += amount
        ccy_st['fees_native'] += amount
        ccy_st['fees_base']    = _add_base(ccy_st['fees_base'], amount, fx_at)
    else:
        # Unknown — credit conservatively as deposit.
        _bucket_deposit(state, ccy, amount, fx_for_bucket, sid, ts_us)
        ccy_st['deposits'] += amount


# ---------- DIVIDEND ----------

def _apply_dividend(state, ev, out_closures, out_cycles):
    gross = _to_float(ev.get('gross_native'))
    if gross == 0:
        return
    withholding = _to_float(ev.get('withholding_native'))
    net = gross - withholding
    if net <= 0:
        return
    fx_fees_d = _to_float(ev.get('fx_fees_native'))
    base = state['base_currency']
    ccy = (ev.get('currency') or base).upper()
    is_base = ccy == base
    is_foreign = not is_base
    fx_rate_t = ev.get('fx_rate_to_base')
    fx_rate_t = float(fx_rate_t) if fx_rate_t is not None else None
    ts_us = ev.get('business_ts')
    sid = ev.get('source_id')

    settled_in_base = (fx_rate_t is not None) and is_foreign
    if settled_in_base:
        rate = fx_rate_t
        base_ccy = _get_ccy(state, base)
        _bucket_deposit(state, base, net * rate, 1.0, sid, ts_us)
        base_ccy['realized_dividends_native'] += gross
        base_ccy['realized_dividends_base']    = _add_base(
            base_ccy['realized_dividends_base'], gross, rate,
        )
        if withholding > 0:
            wh_base = withholding * rate
            base_ccy['fees_native'] += wh_base
            base_ccy['fees_base']    = _add_base(base_ccy['fees_base'], wh_base, 1.0)
        if fx_fees_d:
            fx_fees_ccy = (ev.get('fx_fees_currency') or ev.get('currency') or '').upper()
            fx_fees_fx  = rate if fx_fees_ccy == ccy else 1.0
            if fx_fees_ccy == base:
                base_ccy['fees_native'] += fx_fees_d * fx_fees_fx
            base_ccy['fees_base']    = _add_base(base_ccy['fees_base'], fx_fees_d, fx_fees_fx)
        return

    ccy_st = _get_ccy(state, ccy)
    fx_at = float(ev.get('event_time_fx')) if ev.get('event_time_fx') is not None else (
        1.0 if is_base else 1.0
    )
    _bucket_deposit(state, ccy, net, fx_at, sid, ts_us)
    ccy_st['realized_dividends_native'] += gross
    ccy_st['realized_dividends_base']    = _add_base(
        ccy_st['realized_dividends_base'], gross, fx_at,
    )
    if withholding > 0:
        ccy_st['fees_native'] += withholding
        ccy_st['fees_base']    = _add_base(ccy_st['fees_base'], withholding, fx_at)
    if fx_fees_d:
        fx_fees_ccy = (ev.get('fx_fees_currency') or ev.get('currency') or '').upper()
        if fx_fees_ccy == ccy:
            ccy_st['fees_native'] += fx_fees_d
            ccy_st['fees_base']    = _add_base(ccy_st['fees_base'], fx_fees_d, fx_at)


# ---------- TRANSFER_IN ----------

def _apply_transfer_in(state, ev, out_closures, out_cycles):
    iid = ev.get('instrument_id')
    if not iid:
        return
    qty = _to_float(ev.get('quantity'))
    if qty <= 0:
        return
    base = state['base_currency']
    ccy = (ev.get('currency') or base).upper()
    kind_meta = ev.get('instrument_kind') or 'equity'
    inst = _get_inst(state, iid, ccy, kind_meta)
    _get_ccy(state, ccy)
    cost_basis = _to_float(ev.get('cost_basis_native'))
    fx_at_acq = float(ev.get('fx_rate_at_acquisition')) if ev.get('fx_rate_at_acquisition') is not None else 1.0
    entry_price = cost_basis / qty if qty > EPS_LOT else 0.0
    ts_us = ev.get('acquisition_date') or ev.get('business_ts')
    transfer_id = ev.get('transfer_id') or ev.get('source_id')

    # No cash leg — transfer_in lots are pre-existing.
    was_flat = inst['fifo']['direction'] == 'flat'
    lot = {
        'lot_id':              transfer_id,
        'event_ts_us':         ts_us,
        'quantity':            qty,
        'price':               entry_price,
        'price_base':          entry_price * fx_at_acq,
        'fx_at_buy':           fx_at_acq,
        'commission':          0.0,
        'commission_base':     0.0,
        'fees':                0.0,
        'fees_base':           0.0,
        'contract_multiplier': 1.0,
    }
    _fifo_open(inst['fifo'], lot, 'long')
    _avg_open(inst['avg'], dict(lot))
    if was_flat:
        inst['cycle_open_ts_us'] = ts_us
        inst['cycle_pnl']        = 0.0


# ---------- FX_CONVERSION ----------

def _apply_fx_conversion(state, ev, out_closures, out_cycles):
    from_ccy   = (ev.get('from_currency') or '').upper()
    to_ccy     = (ev.get('to_currency')   or '').upper()
    from_amt   = _to_float(ev.get('from_amount'))
    to_amt     = _to_float(ev.get('to_amount'))
    broker_fx  = _to_float(ev.get('broker_fx_to_base'), 1.0) or 1.0
    fee_native = _to_float(ev.get('fees_native'))
    ts_us      = ev.get('business_ts')
    sid        = ev.get('source_id')
    if not from_ccy or not to_ccy:
        return
    _get_ccy(state, from_ccy)
    _get_ccy(state, to_ccy)
    res = None
    if from_amt > 0:
        res = _bucket_drain(state, from_ccy, from_amt, broker_fx, sid, 'convert', ts_us)
    if to_amt > 0:
        to_fx = (from_amt * broker_fx / to_amt) if to_amt else broker_fx
        _bucket_deposit(state, to_ccy, to_amt, to_fx, sid, ts_us)
    if res is not None and res['borrowed_qty'] > EPS and res['borrow_id'] is not None:
        state['pending_borrows'][res['borrow_id']] = {
            'consumer':         sid,
            'consumer_kind':    'convert',
            'from_ccy':         from_ccy,
            'borrowed_qty':     res['borrowed_qty'],
            'lot_backed_qty':   res['lot_backed_qty'],
            'lot_backed_basis': res['lot_backed_basis'],
            'cover_slices':     [],
            'covered_qty':      0.0,
            'finalized':        False,
            'fee_native':       0.0,
            'from_broker_fx':   broker_fx,
        }
    if fee_native:
        fee_ccy = (ev.get('fees_currency') or ev.get('from_currency') or '').upper()
        if fee_ccy:
            fee_st = _get_ccy(state, fee_ccy)
            _bucket_drain(state, fee_ccy, fee_native, None, sid, 'fee', ts_us)
            fee_st['fees_native'] += fee_native
            fee_st['fees_base']    = _add_base(fee_st['fees_base'], fee_native, broker_fx)


# ============================================================================
# Dispatcher
# ============================================================================

_DISPATCH = {
    'TRADE':         _apply_trade,
    'CASHFLOW':      _apply_cashflow,
    'DIVIDEND':      _apply_dividend,
    'TRANSFER_IN':   _apply_transfer_in,
    'FX_CONVERSION': _apply_fx_conversion,
}


# ============================================================================
# UDAF entrypoints
# ============================================================================

def create_state():
    return {
        'base_currency':         'GBP',
        'n_events':              0,
        'last_ts_us':            None,
        'instruments':           {},
        'currencies':            {},
        'pending_borrows':       {},
        'buy_fx_basis':          {},
        'foreign_sell_fx_basis': {},
        'open_lots_fx_tracker':  {},
        '_last_closures':        [],
        '_last_cycles':          [],
    }


def accumulate(state, event):
    if event is None:
        return state
    new_state = _clone_state(state)
    bc = event.get('base_currency')
    if bc:
        new_state['base_currency'] = bc
    handler = _DISPATCH.get(event.get('event_type'))
    closures = []
    cycles   = []
    if handler is not None:
        handler(new_state, event, closures, cycles)
    new_state['n_events']       += 1
    new_state['last_ts_us']      = event.get('business_ts')
    new_state['_last_closures']  = closures
    new_state['_last_cycles']    = cycles
    return new_state


def retract(state, event):
    # Sentinel retract: RW falls back to full re-accumulate from partition
    # start when the operator detects divergence. Reverse-plan retract is
    # tracked as a separate plan item.
    new_state = _clone_state(state)
    new_state['_dirty'] = True
    return new_state


# ============================================================================
# Snapshot projection
# ============================================================================

def finish(state):
    base = state['base_currency']
    equity_positions = []
    realized_equity_fifo_total = 0.0
    realized_equity_avg_total  = 0.0
    realized_forex_fifo_total  = 0.0
    realized_forex_avg_total   = 0.0
    for iid, inst in state['instruments'].items():
        fifo = inst['fifo']
        avg  = inst['avg']
        if fifo['qty'] <= EPS_LOT and avg['qty'] <= EPS_LOT:
            # Closed: skip (no consumer needs qty=0 markers).
            # But still accumulate realized totals for portfolio_core rollup.
            realized_equity_fifo_total += fifo['realized_gross_base'] - fifo['realized_fx_gain_base']
            realized_equity_avg_total  += avg['realized_gross_base']  - avg['realized_fx_gain_base']
            realized_forex_fifo_total  += fifo['realized_fx_gain_base']
            realized_forex_avg_total   += avg['realized_fx_gain_base']
            continue

        # avg_cost lookups
        avg_cost_fifo_native = None
        avg_cost_fifo_base   = None
        if fifo['qty'] > EPS_LOT and fifo['lots']:
            tot = sum(l['quantity'] for l in fifo['lots'])
            if tot > EPS_LOT:
                avg_cost_fifo_native = sum(l['quantity'] * l['price']      for l in fifo['lots']) / tot
                avg_cost_fifo_base   = sum(l['quantity'] * l['price_base'] for l in fifo['lots']) / tot
        avg_cost_avg_native = avg['avg_native'] if avg['qty'] > EPS_LOT else None
        avg_cost_avg_base   = avg['avg_base']   if avg['qty'] > EPS_LOT else None

        eq_fifo_base = fifo['realized_gross_base'] - fifo['realized_fx_gain_base']
        eq_avg_base  = avg['realized_gross_base']  - avg['realized_fx_gain_base']
        realized_equity_fifo_total += eq_fifo_base
        realized_equity_avg_total  += eq_avg_base
        realized_forex_fifo_total  += fifo['realized_fx_gain_base']
        realized_forex_avg_total   += avg['realized_fx_gain_base']

        equity_positions.append({
            'instrument_id':                iid,
            'quantity':                     fifo['qty'],
            'direction':                    fifo['direction'],
            'currency':                     inst['currency'],
            'base_currency':                base,
            'lot_count':                    len(fifo['lots']),
            'avg_cost_fifo_native':         avg_cost_fifo_native,
            'avg_cost_avg_native':          avg_cost_avg_native,
            'avg_cost_fifo_base':           avg_cost_fifo_base,
            'avg_cost_avg_base':            avg_cost_avg_base,
            'realized_equity_fifo_native':  fifo['realized_gross_native'],
            'realized_equity_avg_native':   avg['realized_gross_native'],
            'realized_equity_fifo_base':    eq_fifo_base,
            'realized_equity_avg_base':     eq_avg_base,
            'realized_forex_fifo_base':     fifo['realized_fx_gain_base'],
            'realized_forex_avg_base':      avg['realized_fx_gain_base'],
            'instrument_kind':              inst['instrument_kind'],
            'n_trades':                     inst['n_trades'],
            'cycle_count':                  inst['cycle_count'],
        })

    cash_positions = []
    cash_value_base_total       = 0.0
    realized_dividends_base_tot = 0.0
    realized_interest_base_tot  = 0.0
    fees_base_total             = 0.0
    realized_fx_fifo_total      = 0.0
    realized_fx_avg_total       = 0.0
    for ccy, c in state['currencies'].items():
        bucket = c['bucket']
        balance = bucket['lot_qty']
        # cash_value_base: weighted-avg FX from running avg (NOT spot).
        # For base currency it's the native balance.
        if ccy == base:
            cv_base = balance
        else:
            cv_base = balance * bucket['avg_fx_to_base'] if bucket['avg_fx_to_base'] else 0.0
        cash_positions.append({
            'currency':                  ccy,
            'base_currency':             base,
            'cash_value_native':         balance,
            'cash_value_base':           cv_base,
            'balance_native':            balance,
            'realized_interest_native':  c['realized_interest_native'],
            'realized_interest_base':    c['realized_interest_base'],
            'realized_dividends_native': c['realized_dividends_native'],
            'realized_dividends_base':   c['realized_dividends_base'],
            'fees_native':               c['fees_native'],
            'fees_base':                 c['fees_base'],
            'realized_fx_fifo_base':     bucket['realized_fx_gain_fifo_base'],
            'realized_fx_avg_base':      bucket['realized_fx_gain_avg_base'],
            'unrealized_fx_fifo_base':   0.0,
            'unrealized_fx_avg_base':    0.0,
            'deposits_cumulative_native':    c['deposits'],
            'withdrawals_cumulative_native': c['withdrawals'],
            'interest_cumulative_native':    c['interest'],
            'fees_cumulative_native':        c['fees_cum'],
        })
        cash_value_base_total       += cv_base
        if c['realized_dividends_base'] is not None:
            realized_dividends_base_tot += c['realized_dividends_base']
        if c['realized_interest_base'] is not None:
            realized_interest_base_tot  += c['realized_interest_base']
        if c['fees_base'] is not None:
            fees_base_total             += c['fees_base']
        realized_fx_fifo_total += bucket['realized_fx_gain_fifo_base']
        realized_fx_avg_total  += bucket['realized_fx_gain_avg_base']

    realized_pnl_base = (
        realized_equity_fifo_total + realized_forex_fifo_total
        + realized_fx_fifo_total
        + realized_dividends_base_tot
        + realized_interest_base_tot
    )

    portfolio_core = {
        'base_currency':            base,
        'cash_value_base':          cash_value_base_total,
        'cash_base':                cash_value_base_total,
        'realized_pnl_base':        realized_pnl_base,
        'realized_equity_fifo_base':realized_equity_fifo_total,
        'realized_equity_avg_base': realized_equity_avg_total,
        'realized_forex_fifo_base': realized_forex_fifo_total + realized_fx_fifo_total,
        'realized_forex_avg_base':  realized_forex_avg_total  + realized_fx_avg_total,
        'realized_interest_base':   realized_interest_base_tot,
        'realized_dividends_base':  realized_dividends_base_tot,
        'fees_base':                fees_base_total,
        'n_events':                 state['n_events'],
        'last_ts':                  _us_to_iso(state['last_ts_us']),
        'instrument_count':         len(equity_positions),
        'cash_position_count':      len(cash_positions),
    }

    snapshot = {
        'equity_positions': equity_positions,
        'cash_positions':   cash_positions,
        'portfolio_core':   portfolio_core,
    }

    return {
        'snapshot': snapshot,
        'closures': state.get('_last_closures', []),
        'cycles':   state.get('_last_cycles', []),
        'dirty':    bool(state.get('_dirty', False)),
    }
$$;
