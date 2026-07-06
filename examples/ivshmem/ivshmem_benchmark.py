#!/usr/bin/env python3
"""
ivshmem Performance Benchmark Tool

用法:
    # 创建新 sandbox 并测试
    python3 ivshmem_benchmark.py

    # 测试已存在的 sandbox
    python3 ivshmem_benchmark.py --sandbox-id abc123...

    # 测试多个 sandbox（并发）
    python3 ivshmem_benchmark.py --count 5

    # 自定义测试参数
    python3 ivshmem_benchmark.py --iterations 20000 --block-size 2048
"""

import sys
import time
import os
import mmap
import argparse
import threading
from concurrent.futures import ThreadPoolExecutor, as_completed

sys.path.insert(0, "/root/CubeSandbox-fix/sdk/python")
from cubesandbox import Sandbox
import requests


class IvshmemBenchmark:
    """ivshmem 性能测试类"""

    def __init__(self, sandbox_id, ivshmem_path, iterations=10000):
        self.sandbox_id = sandbox_id
        self.ivshmem_path = ivshmem_path
        self.iterations = iterations
        self.results = {}

    def run_all_tests(self):
        """运行所有测试"""
        if not os.path.exists(self.ivshmem_path):
            return {"error": f"ivshmem file not found: {self.ivshmem_path}"}

        with open(self.ivshmem_path, 'r+b') as f:
            mm = mmap.mmap(f.fileno(), 1048576)

            self.results['single_byte'] = self._test_single_byte(mm)
            self.results['block_100b'] = self._test_block(mm, 100, self.iterations)
            self.results['block_1kb'] = self._test_block(mm, 1024, self.iterations)
            self.results['block_100kb'] = self._test_block(mm, 100*1024, 1000)

            mm.close()

        return self.results

    def _test_single_byte(self, mm):
        """单字节写入测试"""
        iterations = self.iterations
        start = time.perf_counter()
        for i in range(iterations):
            mm[i % 1048576] = 65
        end = time.perf_counter()

        elapsed = end - start
        latency_us = (elapsed / iterations) * 1_000_000

        return {
            'latency_us': round(latency_us, 3),
            'ops_per_sec': int(iterations / elapsed)
        }

    def _test_block(self, mm, block_size, iterations):
        """块写入测试"""
        block = b"X" * block_size

        start = time.perf_counter()
        for i in range(iterations):
            offset = (i * block_size) % (1048576 - block_size)
            mm[offset:offset+block_size] = block
        end = time.perf_counter()

        elapsed = end - start
        latency_us = (elapsed / iterations) * 1_000_000
        throughput_mb = (block_size * iterations) / elapsed / (1024 * 1024)

        return {
            'block_size': block_size,
            'latency_us': round(latency_us, 3),
            'throughput_mb': round(throughput_mb, 2)
        }


def create_sandbox_with_ivshmem(template_id):
    """创建启用 ivshmem 的 sandbox"""
    sb = Sandbox.create(
        template=template_id,
        metadata={"enable_ivshmem": "true"}
    )
    time.sleep(8)
    ivshmem_path = f"/dev/shm/ivshmem-{sb.sandbox_id}"

    if not os.path.exists(ivshmem_path):
        raise FileNotFoundError(f"ivshmem file not created: {ivshmem_path}")

    return sb.sandbox_id, ivshmem_path


def cleanup_sandbox(sandbox_id):
    """清理 sandbox"""
    try:
        requests.delete(f"http://127.0.0.1:3000/sandboxes/{sandbox_id}")
        time.sleep(1)
    except:
        pass

    ivshmem_path = f"/dev/shm/ivshmem-{sandbox_id}"
    if os.path.exists(ivshmem_path):
        os.remove(ivshmem_path)


def run_benchmark_single(sandbox_id, ivshmem_path, iterations):
    """对单个 sandbox 运行 benchmark"""
    bench = IvshmemBenchmark(sandbox_id, ivshmem_path, iterations)
    results = bench.run_all_tests()
    return sandbox_id, results


def print_results(sandbox_id, results):
    """打印测试结果"""
    print(f"\n{'='*70}")
    print(f"Sandbox: {sandbox_id}")
    print('='*70)

    if 'error' in results:
        print(f"❌ 错误: {results['error']}")
        return

    print(f"\n单字节写入:")
    print(f"  延迟: {results['single_byte']['latency_us']} µs/op")
    print(f"  吞吐: {results['single_byte']['ops_per_sec']:,} ops/s")

    for test_name, result in results.items():
        if test_name.startswith('block_'):
            size = result['block_size']
            if size < 1024:
                size_str = f"{size}B"
            else:
                size_str = f"{size // 1024}KB"

            print(f"\n{size_str} 块写入:")
            print(f"  延迟: {result['latency_us']} µs/op")
            print(f"  吞吐: {result['throughput_mb']} MB/s")


def print_summary(all_results):
    """打印汇总统计"""
    if not all_results:
        return

    print(f"\n{'='*70}")
    print("汇总统计")
    print('='*70)

    # 计算平均值
    single_byte_latencies = [r['single_byte']['latency_us'] for _, r in all_results if 'error' not in r]
    block_1kb_throughputs = [r['block_1kb']['throughput_mb'] for _, r in all_results if 'error' not in r]
    block_100kb_throughputs = [r['block_100kb']['throughput_mb'] for _, r in all_results if 'error' not in r]

    if single_byte_latencies:
        avg_latency = sum(single_byte_latencies) / len(single_byte_latencies)
        avg_1kb_tp = sum(block_1kb_throughputs) / len(block_1kb_throughputs)
        avg_100kb_tp = sum(block_100kb_throughputs) / len(block_100kb_throughputs)

        print(f"\n测试 Sandbox 数量: {len(all_results)}")
        print(f"成功测试: {len(single_byte_latencies)}")
        print(f"\n平均性能:")
        print(f"  单字节延迟: {avg_latency:.3f} µs")
        print(f"  1KB 吞吐: {avg_1kb_tp:.2f} MB/s")
        print(f"  100KB 吞吐: {avg_100kb_tp:.2f} MB/s")


def main():
    parser = argparse.ArgumentParser(
        description='ivshmem Performance Benchmark Tool',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
示例:
  # 创建新 sandbox 并测试
  python3 ivshmem_benchmark.py

  # 测试已存在的 sandbox
  python3 ivshmem_benchmark.py --sandbox-id abc123def456

  # 测试多个 sandbox（并发）
  python3 ivshmem_benchmark.py --count 5

  # 自定义迭代次数
  python3 ivshmem_benchmark.py --iterations 20000
        """
    )

    parser.add_argument('--sandbox-id', help='已存在的 sandbox ID')
    parser.add_argument('--count', type=int, default=1, help='要测试的 sandbox 数量（默认: 1）')
    parser.add_argument('--template', default='tpl-2589b96bc11a42b5b8954c28', help='模板 ID')
    parser.add_argument('--iterations', type=int, default=10000, help='迭代次数（默认: 10000）')
    parser.add_argument('--parallel', action='store_true', help='并发测试（用于多个 sandbox）')
    parser.add_argument('--cleanup', action='store_true', help='测试后清理 sandbox')

    args = parser.parse_args()

    print("="*70)
    print("ivshmem Performance Benchmark")
    print("="*70)

    all_results = []
    sandboxes_to_cleanup = []

    try:
        if args.sandbox_id:
            # 测试已存在的 sandbox
            ivshmem_path = f"/dev/shm/ivshmem-{args.sandbox_id}"
            print(f"\n测试已存在的 Sandbox: {args.sandbox_id}")
            sandbox_id, results = run_benchmark_single(args.sandbox_id, ivshmem_path, args.iterations)
            all_results.append((sandbox_id, results))
            print_results(sandbox_id, results)

        else:
            # 创建并测试新 sandbox
            print(f"\n创建 {args.count} 个 Sandbox...")

            sandboxes = []
            for i in range(args.count):
                try:
                    sb_id, ivshmem_path = create_sandbox_with_ivshmem(args.template)
                    sandboxes.append((sb_id, ivshmem_path))
                    sandboxes_to_cleanup.append(sb_id)
                    print(f"  [{i+1}/{args.count}] 创建: {sb_id}")
                except Exception as e:
                    print(f"  [{i+1}/{args.count}] 失败: {e}")

            if not sandboxes:
                print("❌ 没有成功创建 sandbox")
                return 1

            print(f"\n✅ 成功创建 {len(sandboxes)} 个 Sandbox")
            print(f"\n开始性能测试...")

            if args.parallel and len(sandboxes) > 1:
                # 并发测试
                with ThreadPoolExecutor(max_workers=len(sandboxes)) as executor:
                    futures = {
                        executor.submit(run_benchmark_single, sb_id, path, args.iterations): sb_id
                        for sb_id, path in sandboxes
                    }

                    for future in as_completed(futures):
                        sandbox_id, results = future.result()
                        all_results.append((sandbox_id, results))
                        print_results(sandbox_id, results)
            else:
                # 串行测试
                for sb_id, ivshmem_path in sandboxes:
                    sandbox_id, results = run_benchmark_single(sb_id, ivshmem_path, args.iterations)
                    all_results.append((sandbox_id, results))
                    print_results(sandbox_id, results)

            # 打印汇总
            if len(all_results) > 1:
                print_summary(all_results)

    finally:
        # 清理
        if args.cleanup and sandboxes_to_cleanup:
            print(f"\n清理 {len(sandboxes_to_cleanup)} 个 Sandbox...")
            for sb_id in sandboxes_to_cleanup:
                cleanup_sandbox(sb_id)
            print("✅ 清理完成")

    return 0


if __name__ == '__main__':
    sys.exit(main())
