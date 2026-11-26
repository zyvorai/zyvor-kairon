// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/zyvorai/kairon/internal/ebpfedge"
)

func endpoint(ip string, port uint16) string {
	if ip == "" {
		return "-"
	}
	if port == 0 {
		return ip
	}
	return fmt.Sprintf("%s:%d", ip, port)
}

func anyProcess(flows []ebpfedge.FlowRecord) (guest, host, rtt bool) {
	for _, fr := range flows {
		if !fr.Process.IsZero() {
			guest = true
		}
		if !fr.HostProcess.IsZero() {
			host = true
		}
		if !fr.Latency.IsZero() {
			rtt = true
		}
	}
	return
}

func anyDropMeta(drops []ebpfedge.DropEvent) (proc, rtt bool) {
	for _, ev := range drops {
		if !ev.Process.IsZero() || !ev.HostProcess.IsZero() {
			proc = true
		}
		if !ev.Latency.IsZero() {
			rtt = true
		}
	}
	return
}

// formatFlowTable writes a tab-separated flow table. Process, host and
// RTT columns appear only when at least one row has that data.
func formatFlowTable(w io.Writer, flows []ebpfedge.FlowRecord) {
	if len(flows) == 0 {
		fmt.Fprintln(w, "no flows")
		return
	}
	guest, host, rtt := anyProcess(flows)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	header := []string{"PROTO", "SRC", "DST", "PACKETS", "BYTES"}
	if rtt {
		header = append(header, "RTT")
	}
	if guest {
		header = append(header, "PROCESS")
	}
	if host {
		header = append(header, "HOST")
	}
	header = append(header, "REASON")
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, fr := range flows {
		cols := []string{
			dash(fr.Proto),
			endpoint(fr.SrcIP, fr.SrcPort),
			endpoint(fr.DstIP, fr.DstPort),
			fmt.Sprintf("%d", fr.Packets),
			fmt.Sprintf("%d", fr.Bytes),
		}
		if rtt {
			cols = append(cols, fr.Latency.Display())
		}
		if guest {
			cols = append(cols, fr.Process.Display())
		}
		if host {
			cols = append(cols, fr.HostProcess.Display())
		}
		reason := fr.Reason
		if reason == "" && !fr.Denied {
			reason = "-"
		}
		cols = append(cols, dash(reason))
		fmt.Fprintln(tw, strings.Join(cols, "\t"))
	}
	_ = tw.Flush()
}

// formatDropTable writes a tab-separated attributed-drop table.
func formatDropTable(w io.Writer, drops []ebpfedge.DropEvent) {
	if len(drops) == 0 {
		fmt.Fprintln(w, "no drops")
		return
	}
	proc, rtt := anyDropMeta(drops)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	header := []string{"REASON", "POLICY", "SRC", "DST", "PACKETS"}
	if proc {
		header = append(header, "PROCESS")
	}
	if rtt {
		header = append(header, "RTT")
	}
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, ev := range drops {
		cols := []string{
			dash(ev.Reason),
			dash(ev.PolicyName),
			endpoint(ev.SrcIP, ev.SrcPort),
			endpoint(ev.DstIP, ev.DstPort),
			fmt.Sprintf("%d", ev.Packets),
		}
		if proc {
			label := ev.Process.Display()
			if ev.Process.IsZero() && !ev.HostProcess.IsZero() {
				label = ev.HostProcess.Display()
			}
			cols = append(cols, label)
		}
		if rtt {
			cols = append(cols, ev.Latency.Display())
		}
		fmt.Fprintln(tw, strings.Join(cols, "\t"))
	}
	_ = tw.Flush()
}

// formatDataplaneStats writes counters and any latency the payload carried.
func formatDataplaneStats(w io.Writer, s ebpfedge.DataplaneStats) {
	fmt.Fprintf(w, "rx packets\t%d\n", s.RxPackets)
	fmt.Fprintf(w, "tx packets\t%d\n", s.TxPackets)
	fmt.Fprintf(w, "rx bytes\t%d\n", s.RxBytes)
	fmt.Fprintf(w, "tx bytes\t%d\n", s.TxBytes)
	fmt.Fprintf(w, "dropped\t%d\n", s.Dropped)
	if s.Flows > 0 {
		fmt.Fprintf(w, "flows\t%d\n", s.Flows)
	}
	if !s.Latency.IsZero() {
		fmt.Fprintf(w, "rtt\t%s", s.Latency.Display())
		if s.Latency.MinRTTUs > 0 && s.Latency.MinRTTUs != s.Latency.RTTUs {
			fmt.Fprintf(w, " (min %s)", ebpfedge.FormatMicroseconds(s.Latency.MinRTTUs))
		}
		if s.Latency.MaxRTTUs > 0 && s.Latency.MaxRTTUs != s.Latency.RTTUs {
			fmt.Fprintf(w, " (max %s)", ebpfedge.FormatMicroseconds(s.Latency.MaxRTTUs))
		}
		fmt.Fprintln(w)
	}
}
