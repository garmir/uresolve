package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

type Config struct {
	concurrency int
	timeout     time.Duration
	verbose     bool
	showIPs     bool
	retries     int
	ipv6        bool
}

var config Config

func init() {
	flag.IntVar(&config.concurrency, "c", 100, "Maximum concurrent resolutions")
	flag.DurationVar(&config.timeout, "t", 5*time.Second, "Resolution timeout per domain")
	flag.BoolVar(&config.verbose, "v", false, "Verbose output (show errors)")
	flag.BoolVar(&config.showIPs, "i", false, "Show IP addresses")
	flag.IntVar(&config.retries, "r", 2, "Number of retries for failed resolutions")
	flag.BoolVar(&config.ipv6, "6", false, "Include IPv6 addresses")
}

func main() {
	flag.Parse()

	jobs := make(chan string, config.concurrency*2)
	results := make(chan string, config.concurrency)

	ctx := context.Background()
	var wg sync.WaitGroup

	// Semaphore for concurrency control
	sem := make(chan struct{}, config.concurrency)

	// Start result printer
	go func() {
		for result := range results {
			fmt.Println(result)
		}
	}()

	// Start worker pool
	go func() {
		for domain := range jobs {
			sem <- struct{}{} // Acquire semaphore
			wg.Add(1)
			
			go func(d string) {
				defer func() {
					<-sem // Release semaphore
					wg.Done()
				}()
				
				processDomain(ctx, d, results)
			}(domain)
		}
	}()

	// Read domains from stdin
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		domain := strings.TrimSpace(scanner.Text())
		if domain == "" {
			continue
		}
		
		// Clean domain
		domain = cleanDomain(domain)
		if domain != "" {
			jobs <- domain
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error reading input: %v\n", err)
	}

	close(jobs)
	wg.Wait()
	close(results)
}

func processDomain(ctx context.Context, domain string, results chan<- string) {
	ips, err := resolveWithRetry(ctx, domain)
	
	if err != nil {
		if config.verbose {
			fmt.Fprintf(os.Stderr, "Failed to resolve %s: %v\n", domain, err)
		}
		return
	}

	if len(ips) == 0 {
		if config.verbose {
			fmt.Fprintf(os.Stderr, "No IPs found for %s\n", domain)
		}
		return
	}

	if config.showIPs {
		results <- fmt.Sprintf("%s [%s]", domain, strings.Join(ips, ", "))
	} else {
		results <- domain
	}
}

func resolveWithRetry(ctx context.Context, domain string) ([]string, error) {
	var lastErr error
	
	for attempt := 0; attempt <= config.retries; attempt++ {
		if attempt > 0 {
			// Exponential backoff
			time.Sleep(time.Millisecond * 100 * time.Duration(1<<uint(attempt-1)))
		}

		ips, err := resolve(ctx, domain)
		if err == nil && len(ips) > 0 {
			return ips, nil
		}
		lastErr = err
	}
	
	return nil, lastErr
}

func resolve(ctx context.Context, domain string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, config.timeout)
	defer cancel()

	resolver := &net.Resolver{
		PreferGo: true,
		StrictErrors: false,
	}

	// Resolve IP addresses
	addrs, err := resolver.LookupIPAddr(ctx, domain)
	if err != nil {
		// Try with system resolver as fallback
		ips, err2 := net.LookupHost(domain)
		if err2 != nil {
			return nil, err
		}
		return filterIPs(ips), nil
	}

	// Convert to string IPs
	var ips []string
	seen := make(map[string]bool)
	
	for _, addr := range addrs {
		ip := addr.IP.String()
		
		// Skip duplicates
		if seen[ip] {
			continue
		}
		seen[ip] = true
		
		// Filter based on IPv6 preference
		if !config.ipv6 && strings.Contains(ip, ":") {
			continue
		}
		
		ips = append(ips, ip)
	}

	return ips, nil
}

func filterIPs(ips []string) []string {
	var filtered []string
	seen := make(map[string]bool)
	
	for _, ip := range ips {
		// Skip duplicates
		if seen[ip] {
			continue
		}
		seen[ip] = true
		
		// Filter IPv6 if not requested
		if !config.ipv6 && strings.Contains(ip, ":") {
			continue
		}
		
		// Skip local/private IPs
		if isPrivateIP(ip) {
			continue
		}
		
		filtered = append(filtered, ip)
	}
	
	return filtered
}

func isPrivateIP(ip string) bool {
	// Parse IP
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	
	// Check if it's a private IP
	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}
	
	for _, cidr := range privateRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(parsed) {
			return true
		}
	}
	
	return false
}

func cleanDomain(domain string) string {
	// Remove common URL prefixes
	prefixes := []string{"http://", "https://", "ftp://", "ws://", "wss://"}
	for _, prefix := range prefixes {
		domain = strings.TrimPrefix(domain, prefix)
	}
	
	// Remove path
	if idx := strings.Index(domain, "/"); idx != -1 {
		domain = domain[:idx]
	}
	
	// Remove port (but not for IPv6)
	if !strings.HasPrefix(domain, "[") {
		if idx := strings.LastIndex(domain, ":"); idx != -1 {
			// Make sure it's not part of IPv6
			if !strings.Contains(domain[:idx], ":") {
				domain = domain[:idx]
			}
		}
	}
	
	// Remove trailing dots
	domain = strings.TrimSuffix(domain, ".")
	
	// Clean whitespace
	domain = strings.TrimSpace(domain)
	
	// Basic validation
	if domain == "" || strings.Contains(domain, " ") {
		return ""
	}
	
	return strings.ToLower(domain)
}