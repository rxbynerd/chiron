---
query: |-
  I'm selecting a sub-GHz radio for a wireless M-Bus (wMBus, EN 13757-4) RECEIVER/
  gateway that streams utility-meter readings to the cloud. I need a "radio waves to
  decoded link-layer APDU" bridge ONLY — a host application (mine) already does the
  OMS/AES decryption and DIF/VIF parsing, so I do NOT want or need an on-chip OMS or
  crypto stack. The radio must deliver complete, de-whitened, 3-of-6-decoded (for
  T-mode), CRC-checked wM-Bus link-layer frames to a host over UART or SPI (a
  Raspberry Pi today), which I then JSON-wrap onto MQTT. This must be a transparent
  pass-through, like a Würth Metis-II module in its raw-frame "read" mode.

  HARD REQUIREMENTS:
  - 868 MHz only (EU). 169 MHz and 433 MHz NOT required.
  - Must reliably receive wMBus mode T (and ideally C and S) at 868 MHz. My meters
    include Honeywell V210H (water), Apator Ultrimis W (water), and an electricity
    meter — confirm typical EU/UK transmit modes for these.
  - INTEGRITY IS PARAMOUNT. Acceptable options, in strict preference order:
    (a) the radio/PHY firmware is available as SOURCE CODE that I can build and own
        (vastly preferred), OR
    (b) the device runs CRYPTOGRAPHICALLY SIGNED firmware with verified/secure boot
        (marginally acceptable).
    REJECT anything that is an opaque, unsigned binary blob with no source (this is
    exactly why I'm moving off the NXP UBX100, whose Radio Modem firmware is an
    unsigned, source-unavailable blob).
  - Transparent raw-frame output over UART/SPI; no mandatory on-chip OMS layer.
  2. ST STM32WL55 / STM32WLE5 — full-source + secure-boot SoC. (Medium confidence; veri
  Integrated sub-GHz (Cortex-M4 + SX126x-class radio). STM32Cube is full BSD-3 source, ne + SBSFU/TF-M for signed firmware update. The radio does (G)FSK; wM-Bus S/T/C are(G)FSK + 3-of-6, so it's capable — but you may implement more of the T-mode PHY/coding yourself than on TI, depending on what packaged wM-Bus code exists today (the thing to verify).

  3. rtl-wmbus on an RTL-SDR — maximal integrity, fits your topology. (High confidence nce for your meters.)
  Your architecture is literally "a Pi reads a radio and publishes to MQTT." rtl-wmbus is a 100% open-source software demodulator for T/C/S modes on a cheap RTL-SDR dongle — zero vendor firmware blob anywhere, fully auditable end to end. For a fixed gateway/concentrator (not a battery endpoint) this is a legitimate, integrity-maximal option, and it drops cleanly in front of Miara (it already emits raw hex frames; wmbusmeters consumes the same). Worth a same-day bench test on your three meters.

  Lower down: a CC1101 + MCU with one of the open wM-Bus firmwares is fully source-available and cheap, but you inherit the PHY-tuning burden (closer to your CC112x experience, though far better documented and community-supported).

  Net: if you want a productizable chip-down endpoint, CC13xx SoC is where I'd point first; if you want an auditable fixed concentrator fast, rtl-wmbus. Either keeps Miara unchanged — just preserve the
  "CRC-stripped link-frame hex over MQTT" contract (you'd port the small Pi-side bridgeto the Würth dongle).

  Deep-research prompt (paste into a Claude Research session)

  Since current availability, stack-licensing specifics, and "is the firmware signed" claims really do need fresh primary-source verification, here's a self-contained prompt:

  I'm selecting a sub-GHz radio for a wireless M-Bus (wMBus, EN 13757-4) RECEIVER/
  gateway that streams utility-meter readings to the cloud. I need a "radio waves to
  decoded link-layer APDU" bridge ONLY — a host application (mine) already does the
  OMS/AES decryption and DIF/VIF parsing, so I do NOT want or need an on-chip OMS or
  crypto stack. The radio must deliver complete, de-whitened, 3-of-6-decoded (for
  T-mode), CRC-checked wM-Bus link-layer frames to a host over UART or SPI (a
  Raspberry Pi today), which I then JSON-wrap onto MQTT. This must be a transparent
  pass-through, like a Würth Metis-II module in its raw-frame "read" mode.

  HARD REQUIREMENTS:
  - 868 MHz only (EU). 169 MHz and 433 MHz NOT required.
  - Must reliably receive wMBus mode T (and ideally C and S) at 868 MHz. My meters
    include Honeywell V210H (water), Apator Ultrimis W (water), and an electricity
    meter — confirm typical EU/UK transmit modes for these.
  - INTEGRITY IS PARAMOUNT. Acceptable options, in strict preference order:
    (a) the radio/PHY firmware is available as SOURCE CODE that I can build and own
        (vastly preferred), OR
    (b) the device runs CRYPTOGRAPHICALLY SIGNED firmware with verified/secure boot
        (marginally acceptable).
    REJECT anything that is an opaque, unsigned binary blob with no source (this is
    exactly why I'm moving off the NXP UBX100, whose Radio Modem firmware is an
    unsigned, source-unavailable blob).
  - Transparent raw-frame output over UART/SPI; no mandatory on-chip OMS layer.

  CONTEXT:
  - Current working prototype: Würth Metis-II (closed blob) over UART -> MQTT.
  - I previously tried the TI CC1125/CC1120 TRANSCEIVERS and struggled (implementing
    PHY/MAC on an external MCU). I'm open to TI CC13xx SoCs if they're materially
    easier.
  - This is a fixed/mains-powered gateway, not a battery endpoint, so SDR-based
    receivers (e.g. rtl-wmbus on RTL-SDR) are acceptable if RX performance is adequate.

  PRODUCE:
  1. A comparison matrix of candidate radios/modules/SoCs/SDR options for an 868 MHz
     wMBus T/C/S RECEIVER, with columns: part/approach; integrated MCU vs transceiver
     vs SDR; wMBus mode T/C/S receive support and whether it's turnkey or
     self-implemented; firmware model (open source / signed-secure-boot /
     opaque-unsigned) with EVIDENCE and links; secure-boot / signed-update support;
     does it output raw decoded link-layer frames over UART/SPI out of the box;
     current orderability + rough unit price; ecosystem/maturity for wMBus RX.
  2. For each, the SPECIFIC EVIDENCE for the firmware-integrity claim (SDK license,
     secure-boot docs, signed-update mechanism) — cite primary sources (vendor docs,
     datasheets, SDK licenses, repos). Be explicit when a claim is unverified.
  3. Cover at least: TI CC13xx SoCs (CC1310/CC1312R7/CC1352P) + SimpleLink SDK and any
     TI wMBus software; ST STM32WL55/WLE5 + STM32Cube (secure boot / wMBus PHY
     availability); Silicon Labs EFR32FG2x + RAIL/Connect (and whether wMBus RX is
     open or licensed); open-source firmware on CC1101-class transceivers; and
     rtl-wmbus on RTL-SDR. Note any maintained OPEN-SOURCE wMBus PHY/RX implementations
     for these parts (e.g. on GitHub) and their license.
  4. A clear recommendation ranked for my integrity bar (source >> signed >> reject),
     factoring effort to reach a working T-mode raw-frame receiver, and what I'd reuse
     vs rebuild from a Würth Metis-II-style integration.
  5. Flag anything that needs hands-on verification (RX sensitivity for dense T-mode,
     3-of-6 decode quality, whether a vendor's "wMBus support" is RX or TX-only).

  Verify claims against primary sources; distinguish established fact from inference;
  be explicit about confidence and what you could not confirm.
agent: deep-research-max-preview-04-2026
interaction: v1_ChdsSk1vYXZEcUpidXdrZFVQNzliTjhBdxIXbEpNb2F2RHFKYnV3a2RVUDc5Yk44QXc
status: completed
started: "2026-06-09T22:28:37Z"
completed: "2026-06-09T22:28:37Z"
tokens:
  input: 504903
  output: 39337
  tool_use: 1180930
  thought: 90349
estimated_cost_gbp: 3.95
tools: [google_search, url_context, code_execution]
sources:
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGH2XVn7bhmZmbPEdk5bXrJ4iofl4xp-R9KDHQKWlQLsrGTKhcV_88VcwJq85b5xFQgiGsUWy-na5uH_dJqh1eET_e4bxPYExvkroHdjUNnuUgaHuXP9O7kqqiK9MDS9OjGHldnqwnssjHKj7J-VuqRoT5L8ie9i2WZQzZCxXRZnklQKz3osbFTL-jhQIe3Zk4I_y6zztHl4rP3uzVBzctbbeumlKZ5CJKK_nZpw7S7CE9Eqg==
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEXxIOK7Mv_dN1Ku4ySmBLN01IIH9oC5bixjSjk9PqmZ3OUmRqJNb7UyarF3dVrTbw9Sb6lBjuB5MS38Q6oRqJFl-3MsR6NDA5xGaHTw2aRBUSUjY0syVzEqaRJsQN4Yx6ruLQi70ZkCbQbPtztqtdNON2WL_Zh2CFalX3Q6CUjiUVk
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEjVC19zRnEbYa6Wr5JfEumWtBVPKdDUEfkp_uy2QlLoj4R4X4kpQlQYmMrpF4SYfmceo06yzZO-L5Xa_LW_Wh_x8vd4zX-fA2VV20aERrCn_HInJ4mqUlWPCxp2hgv6aJ8pPLGrRyLNzoqZJbKqgZp4P74gduLEYSQcliYo_SGxt1IzkBk
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQE8eC9y950PmYV8nhg29Mc0r7E7hTBYmF9i7iMU6Hsx3jT5JD65ZeJxVR7cldjrFjrIgqpxTnDieZ6ubpTIBGl4COc6O4DmwTSKsACzbN9TAearx2tf6UepoTb-n3162qNzf82FFOpGBmTZeyAy7ciUFP60qH6cx0jNbsvaXYctCdnqxEwms5AYQnWCu3nfngsHRwHYwM0IiA_4oo7XLy4qkjH31xXwK8W3
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQELY9Ib-L_osbvujPlUh8MyVuCfhv6gMmxHFjJ-scYdG38QYLfGSxj8Ld3825FnPu3b1nfRQkxDaDQh6J3I22qn03CZSyaVzyWR8reuAx4q_puERPB09w4W_OO4gSTBBn5djR2h-6u9f1nNdXdx-TnG2u6OlnXhYL5HVuCp
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEqrZ1rt9jxdmiMYXr1z0EiIDX16MO4CjsuBuoSTrhJ6UVqw3i6L61IiCKCJgSY4b7xBHs_K2GBUdVyQPs9h8aIzcAUs5doA6DMz7VV6Si9UdDVI2iryqTbXpoBRauRmDdYC5HTbL8xOyDvWI3OJLn00N79QNcq
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHl2yGUFElnVwSx_L8GvatXVBfnFy2Lm2JPcRt0SUo5BoP1YYkQK-FAiuO3T-RoGWvdc8W24a4VPavIsma8Mo6aywC0sBvO6Hr3HrFVNIXE5GbJkYofG3UMr9h3QdjW6hP3CHWpoMEhlxWJrkBOYrW7vg==
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFR9FtyGRVoLdgcTkLIGanzc_6xdkBTzvzn3B4H6xD44nzHrF7LeWzVeCQR9QGdpkVlEnDXPaPB-vQSZ3N8EblA6X8ZGCBR3SQqvjMnxbAz9cSZyV32joeN7L-CQ2YiVpt-mpNkYZ9rQjKFLEmWtCdBMHRvf-k=
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFixxNV7Amh7Ib0ElDSZUFoN0XB9qQ2jYaR4EWoenmouxlCOZ4FAD-Gatg070VybHtFpwq_CqUeXePD2xaAt9PSOWLzPG6H9I8UCcoxH3Jg6JGyxB0y5Dfx4BFbh2z7Z73yDnwFZxOZMv0=
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQE6oWCODx06u87Ms-e4FtrwmzDtzCdvhbsLIkCXH6-c6pWQj-y3Uod2AzqO1ae72_y8vGLluQQq1X-aMiOXFx2hJFvGEuFR9aA_vXjNJuysacBl1nrIDln5plx_R96lFoeYVB-FLAPqHJUsGtN9iEj6NT3S_vrGtMtkFXzE1Niv7jc=
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEAUgFDfVvXAwIntk-vQPRwYS33P1WSgdeBv0-d18UjJvUj85zGOlnwDCcfn2vlN31nvW3NOujEg6_aLgApd9djIKfLmRtOX4wznHVMz1CF1EMO9A9kLmVpkC7Pa4kmYYz44v_J79rWcO3I
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHuvaS2HPUkvR2nYfV0zN1eWePS91418vwDR2JVJWbPnJteihnJkCfwjDL3IZnL_TEXnCdsiwNV5jT8JwTPoh4oTfHllNCNT32rIZ1Jo_bo_k3bZAIPiEmYwLqQxYfDtyRBzwj4GgDp0wckCi72k9EiJ95Lii_3Xa8faBwNnfMx49pQeAmotVtfnS41yWrWYViNSN6SAIhKQWOkFZ6oAL2WzlA3a7J_BQV1aC1WCBeypw==
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEHPyWzpe9mClA-xyptEIBtP35XnMC2grOG2wl9oIlIkRHqhOrOS-0EnyQzLoOrRj0iVSkAjWKr2JHvkI8-2ejr0l6xRg08gMD9H2LnY7BBrZYE_N_CqyEnJ6hPkEHq1Sb_Y02ooWF_ktxYhKQsB1HyWD9Wf7abK2Nnw7EmNE3TPp3Il2xS3YLf8Q==
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGoOB7wuMf7x18eSQ7lFqlb1oQxR1tn85NB6fOSwqX2Cf6A4Ek4IwOn2d5AFHjEBdIM1g4h0RmlOOimgJGBPrqzA7hollvrqKceD8dBannZPocsMx0s4oKqT4zU7CT5U9YAnm3rL2Skhk_Rm4RHrW2ieU2DpHstuYg33nDlb0cnIrEC37dkXVOnFQ7LgKG0Ruzt6tubtxclcYNvDYIQ1yxcHvWxGlsvXg==
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHTa2x6IY4WLftpH52JzOL2VfO19_AVG_FGMYRmh1mkrfSnSIhYtee42MIkgd77PDzYGqluNDuvsdN6VRZIngH1myaYZ5R_03umdCEJJ9xWx9WSv-utr-eSmK-2eAkLj8tMPz0Gm4EhaRLgM3mfjMX6z9MwcsIrqm4gCmqm5DEtcm2_e1zNBZIw74bLhT_X4yTQ_D7YKHipIEAh5V-w7Q==
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFByUI9yRw4NgA3xRrcjHvP-7y1tW35h51hmOI8JkXArommQpyO4I77YokvpNiiAGhL7kU6GBjW9R0SJ_Eyc9AqfeVglMFdC9BWhQ6eEHe8KP_k5h1TkswSDpZzugggx57hEeYIZX4ZgJRZiuSUNBeym7ahnD1TLLSJXH-0_p4V-H3y6gMiA7lENY25gYUKeWw7VOZRyBh93ozaGdWcxaY3aquUjcLzWD3wQttj16F8Y_IejEIX_740CA==
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFlXlZ8uFkM2wwXCUActUtHM7j_OU6fk2f_dWDErufQiFijGUU0ESPobUnn6KVmTndpXpKVu-v5WaqJqsnTWkbPwn9DJQFMs1Rd_bL1xatBAjKgVqMgsfSnhcg7jgT323Z60ZwuQ6aLdqiOJGKxKLtyKrh5V9MDrbuRkYjdB4jSn7S7RHKN3f3AqltnN6bPUOpe
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGsYPJVvFhbX1XZtPBqsA2GaJ4lcHPtoiHC03PuQZEDT_u0fID0mq2UFSyI7R0iiW0Plh0up5jzjowubzNYRszZSnsMdYTlVlhr5_qqBDeukx504-YgCZ4=
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQG87nzYrOHpWtuWzuIlQQ0H6HntUjYK2VtWP1aN05yVcjIO9_0OpMcXEIChrtyeEyyj65DF8E8IndHyyzcxvU0P5ad_q8ttrlqCGFPQ6hSfKVSy
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHNFNGZgkIBqZqFx6BFdpnO7dd-nxLkJT2XXBuTZs556431fgzRMehS5lopVXMLa8Mw4e7xTOPQq8LPgDMneH3PxjnWV4zY1vDU86RA6RqkhyX1lYh1O3hWLjuiuwtzRUC2UIMGCQUXBIl2i4BrVNGt51rVBSRV3qVrd_0A2CySYEOAqwCRciiE1bzF9-n6HItqwfxxfokrjlJBIroJFUbwYH9pdDrjj2MRioF8c5kTPgjmXfzidUBMkoB118Q=
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEGiH7jhyH28Z4pW_SqOeeQyFikTiXmq7PqOtQiK1gppp13kxHNLUQzHu9dSkJUHCytGyq1ruXu-AXaOccOCN82kEcXYsPn_QztQl2PWO6rZ-6i0tOt1IeAQmvKXgxGJRnLbfnocaQB2P95dL69eNByjW2KJw==
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFAZvTVK4_mqZFuBqLA25jZhS-razbi9tWVR-rvDz4tCyxd6mSK--KhLqL6gSlvi5P-XCk-jMDOrectOUHYnEQGUDbuBwYqLmuoP0XGqd1qxbEaqlgTtjRcpB-iNHdcJNN38AgTBjMrFSknt1ec7Za3FvGGtBw-Gonw88eE
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGtdhbDyz-8LcbA8nBnsLfOXhjhZkyWWDu8ko4oYoF1GRwSKY9Puh8jio2CVyL_HTWtcDsgvLGYCDuHHO_4qbpXeGEVztTVn7SW0NcJnkfdcxx6Ti0rzEfOc6xT4UUSlyjylh1M
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGltdoNBpXz2ZpzC3Wqa2b6aPOlBAgsiQHHMNVSojs33Vz6qFefpR1W_rWTNGjlA0EzdK8G1ZM41BNMlMMB7elzFSbZioQxYgaMXbNyAuBIjkGWAPpaTdX5Dcwb3T46rMKKlXNFacIMz4TaKoe5GHxn5ghHHztqgx0UeS_uWKJR_Hs-CUAOtZpxpuLznQdcK90=
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFmkU01rmo1aulY66b8xOby80HUcZ1mt1k6RuzzgozbGCDODn6rpBPzC9rvjmEjc6toDxXyldANxC-d6-HTclcknL-fXhOxcpevD0_9FL8FjjFmecgXosd-OdfsqX2xfHM=
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQF3OWV6xIEKaoWCEeLnLS26MiV88qCoRce_deYz5jFFlVr4vbH19hWyV7EfufxdDzP9oYQy5QLuAIZQaWOCnBniHnBJB-7Ez0j0RgN-853Ba0NI4wrEAi895vifByQJSnZwwSVepgZSoiG0IQhOt9_56PgmkaFyFdTDytG1q8dnIn4wIsM=
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHjR1xQFZspZwBX2sBnD4J1brwMcZlAuJjnASlxGTgCcW3E_l4UZIipfMFUBMDR7PfQh4YDeKYiPioXLI6BmUCut80i-P-oyiUAkd0uOUXB8gTxq4GswcCzvv16YQjOvIVLECD0k1lvyH62alCOmphPlq0f0TGhDY5W_yUuQ-CI8pdEfZISzGR0WAiA2o1xtOQ=
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEdTfi8b-wJfjbok5APPl2M1Z4wXoUav7NL7lmzRwilnwM535R_JvKasWYror8LYmOANq_39Fpz3FBnm2c4ClmEA3IJ9senu7_6bmgdVXfq7m3FLh-RKxwEKm3KVhu3oB8=
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGjh-i8lbW-AdBW22VFsLvYOUWtEc4wPTX-Ea8IhYGHMTZ72bHLzhibHyq0VkZLRJLBOVLTIxJYc7VT9Q5f-1Vi6cknfOs82tMNWZg8_3IJ44qttsDOXHJGsHkJ
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGjVnuSOZ3KwjMiAxEs99ieQtUe6bntrVHwfJeWib1zKh5LyK5MZddBfNXOn8OJWlgAhrqUB9DJhXkJwkTeKpd4kJC26fYGrdjxWznPJHRzEurhOiC6daBF4MpgxSEihOv8wxaX_tenPGUYZJkxk4-AdyiXmJFVaWjpWbQo7dtyNQ==
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQF9VSKnF-JcjLZgrFoEb7zYCRZnftCW89U3YEeuG9lXfGytnJAMazg6fJ4QIzv0wMctsvmhhehfyyQs2IMbYAbYKqYj-saBnZY47p0MM6DrAq9uvuyKJtI46HwyVGYLpw==
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQH1oFXIZ5UbkQlCxkP5BUqHtpVOfDuYKezRhHWKGtL4iMC4dAZ0SNCZc4ke1gc7AcP6_4Lwr1Zjkl24p6HynZnLryU_i2m1sXIIAvHfmcRHdS18mjpSyN_gqXRMp05qXnse2-wAoTPN3stm37eYQoRUOqlW5bUNaZh2FzcUGuSzJvIYSC0XpuKAT1kJi346sZl-l30=
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFkEaj7e8a5y7_EId4MED6Tw5F6fSs5D34kITK2aVABJb7RDSw10Annxun_9HobzXVsVaVpoco3tgdcWCMt0T-QO0tAOzIgexLooeyz9RwI-s6qpqwZgshZgM9nSMXf
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGKATwh5r6dSppwzjUffZiWPfMM_RCoVKIhXhcxAhtOedzUMOcKWuQWYR1t45neYPBrbUbwfoyRF4cHukKkNcQLLV1ipivbJ6mGOolSAWwaBx_r99OqTAx9g0lmt81lccTWSsveKd7x
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFKBODw6qei-G2QuBPRPCqc9B8t2MeX8pdkebJ0hyHJIOJRHswFxu9NMS3L_2oguCNAcw1ppBgp6LbJzsRjNGvJ0gAo0Wjvec1rEJFl0Y5p7rWHYrAJ9l_hfeM8f1-rq2vwPKs8s04r
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGtUjO-1yDNAiGdUu5DSbUq5SOsqIPefxfhcZStq8t4DThOHaCSHc8J3HmLmXBH-OXmIJnGXj5d-M2nxlvl1cnV_UgkbEKXP2fUGfdCYHXNojIA2Y6hQjYFz32YhlNOKGAEuGAHCRvUOsK9d2LuFuC69i0x
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQF7f0_kG9dT9YRRahgjpXrzutR3mju78yjkk7hYWEB6-tuSyLdloIkItySQjSCO5qWguNEiBUGtLlwN4eNnJvDTTAvN4toVH_U05Wv3eHu644i_6g2b
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEuTuBvxYjADeLLPBa-sJ1dBotDjagMOroXB7k0MDp6Rxelz-cgtjahcRmP0PPwhMvBVAHOE-CxlkntMiWvzk70ucn-9tPQViXi2sSWY2ZV9ivDUKN4PunQy38cmywo2Zbh-4TIBqrn43QMnsm5zNqOxDRI
  - uri: https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGW7iXOrvo57MeYhz4MB_D2fUlGMKzYLDyjPXUQM06UK59-Y3Zi7j8vMmxSmM76BtO0CbOANQocuv1TJ2kY8c8oYYiKEMPgDx9WG9qbaJi6YOFkc4QSUJtEuTx97J-acxp9d2WwFD7DGnEhYlsAONYFUYvzh-W6u-OxZQ==
---

# Sub-GHz Radio Selection for 868 MHz Wireless M-Bus Gateways: An Integrity-Driven Analysis

## Executive Summary

*   **Open-Source Superiority for Integrity:** Software-Defined Radio (SDR) implementations and legacy transceiver architectures (like the Texas Instruments CC1101) offer the only truly transparent, fully auditable source-code paths for Wireless M-Bus (wM-Bus) reception, natively satisfying stringent firmware integrity requirements without proprietary obfuscation.
*   **The Vendor Ecosystem's "Blob" Problem:** Modern System-on-Chips (SoCs) from major vendors (such as Texas Instruments, Silicon Labs, and STMicroelectronics) heavily restrict Open Metering System (OMS) and wM-Bus stack source code. They universally distribute critical Physical (PHY) and Media Access Control (MAC) layers as opaque, pre-compiled object libraries to protect intellectual property, explicitly violating the requirement for open or auditable firmware. 
*   **Hardware Security as a Fallback:** For productized, chip-down designs, microcontrollers like the STM32WL55 provide robust, cryptographically verified secure boot capabilities, but exploiting these without relying on closed-source vendor radio libraries demands a high-effort, self-implemented PHY layer.
*   **Final Recommendation and Meter Compatibility:** The `rtl-wmbus` software running on a standard RTL-SDR dongle is the definitive recommendation for a fixed gateway, requiring zero opaque blobs and offering turnkey Mode T, C, and S support. This explicitly covers the Apator Ultrimis W (confirmed Mode T1) and standard electricity meters like the Kamstrup OMNIPOWER (Mode C1). Furthermore, using a virtual port bridge like `socat`, the SDR's output perfectly mimics the requested Würth Metis-II UART pass-through behavior.

The integration of utility metering into modern cloud-native architectures frequently encounters a severe friction point at the radio interface layer. Engineers requiring a transparent bridge—converting radio waves directly into decoded Link-Layer Application Protocol Data Units (APDUs) without mandatory on-chip decryption—are often forced into using closed-source firmware blobs. This report investigates candidate sub-GHz receiver architectures operating at the 868 MHz European band, specifically targeting the reliable reception of wM-Bus modes T, C, and S. 

The primary analytical lens of this report is firmware integrity. The assessment strictly prioritizes solutions offering auditable source code, marginally accepts devices capable of cryptographically signed secure boot, and actively rejects unsigned, opaque binary blobs. By comparing hardware transceivers, integrated SoCs, and SDR approaches, this analysis frames the technical trade-offs between RF performance, ecosystem maturity, and firmware transparency.

## Utility Meter Transmit Modes and Frequency Context

Before selecting a radio architecture, it is critical to establish the exact RF behavior of the target endpoint devices. The EN 13757-4 standard defines several wM-Bus modes operating at 868 MHz, each with distinct modulation and timing profiles. 

Research indicates that the Apator Ultrimis W ultrasonic water meter operates explicitly in wM-Bus OMS Mode T1 at 868 MHz [cite: 1]. The device features an integrated radio transmitter with a power output of 16 mW and applies OMS-standard encryption at the device level [cite: 1]. Furthermore, operational field data suggests that the Ultrimis W transmits data at intervals ranging from every 1 to 5 minutes, constrained by its battery-powered profile [cite: 2, 3]. Precise, real-time transmit mode documentation for the Honeywell V210H water meter is unavailable within the primary sources reviewed; however, the vast majority of modern European battery-powered water meters utilize Mode T1 or C1 to conserve power through short, high-speed bursts (100 kbps for T-mode).

While generic mains-powered electricity meters frequently utilize Mode S or continuous C-modes, it is vital to assess specific models for accurate PHY configuration. For example, the Kamstrup OMNIPOWER electricity meter explicitly transmits using Mode C1 (employing AES-128 CTR encryption) at 868 MHz, emitting data telegrams with intervals as frequent as every 16 seconds [cite: 4, 5, 6, 7]. Conversely, another common EU model, the Iskra AM550, is capable of wM-Bus but often relies heavily on Smart Message Language (SML) over a local Infrared (IR) interface or a physical P1 port for extraction, meaning that direct 868 MHz wM-Bus reception is heavily dependent on the local utility's provisioning and module configuration [cite: 8, 9].

The challenge with T-mode reception, particularly in dense urban environments, is the requirement for rapid 3-of-6 decoding and de-whitening to recover the payload without dropping concurrent transmissions. A successful gateway must listen continuously, applying minimal processing overhead to pass the raw, Cyclic Redundancy Check (CRC)-verified frames directly to the host processor.

## Comparison Matrix of Candidate wM-Bus Receivers

To systematically evaluate the available hardware and software combinations, the following matrix compares candidate radios against the strict requirements of transparent T/C/S mode reception, firmware integrity, architectural approach, and current market parameters.

| Part / Approach | Architecture | wM-Bus T/C/S RX Support | Firmware Integrity Model | Secure Boot Supported | Out-of-the-Box Raw UART/SPI Output | Current Orderability & Est. Unit Price | Viability & Maturity |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **RTL-SDR + rtl-wmbus** | SDR Dongle + Host CPU | Turnkey (T1, C1, S1) | **Open Source** (100% C Code) | N/A (Runs on Host OS) | Yes (Outputs raw hex streams via stdout to PTY) | Actively Available (RTL-SDR Blog, AliExpress); ~$30.00 - $39.95 [cite: 10, 11] | High maturity; community-driven |
| **TI CC1101 + ESP32/MCU** | RF Transceiver + External MCU | Self-Implemented / Open Library | **Open Source** (C/C++ libraries) | Dependent on Host MCU | Yes (via open firmware like ESPHome) | Actively Available (Mouser, DigiKey); ~$3.70 - $4.50 [cite: 12] | Mature but requires heavy PHY tuning |
| **ST STM32WL55 / WLE5** | Integrated Dual-Core SoC | Turnkey via Commercial Blob *or* Self-Implemented | **Opaque Object Code** (Stackforce) *or* Open Custom Code | Yes (Hardware level SBSFU / TF-M) | No (Requires firmware abstraction) | Actively Available (DigiKey, Mouser); ~$10.70 - $11.40 [cite: 13, 14] | High hardware viability; software restricted |
| **Silicon Labs EFR32FG2x** | Integrated SoC | Turnkey via RAIL SDK | **Opaque Library** (RAIL abstraction) | Yes (Secure Vault) | No (Requires custom bridging app) | Actively Available (Mouser); ~$7.04 (Qty 1) [cite: 15] | High RF performance; opaque abstraction |
| **TI CC13xx (CC1310/CC1352)** | Integrated SoC | Turnkey (via TI Stack) | **Opaque Object Code** (TI/Stackforce) | Yes (CC1352 only) | Yes (via "Serial" mode hex output) | Actively Available (Mouser, DigiKey); ~$6.41 - $8.88 [cite: 12, 16] | Disqualified based on strict integrity rules |

The data presented above reveals a sharp dichotomy in the sub-GHz radio market. High-performance, modern SoCs (from Texas Instruments, STMicroelectronics, and Silicon Labs) inherently rely on closed-source, pre-compiled object libraries to manage complex RF modulations and regulatory compliance. Conversely, legacy transceivers (CC1101) and SDR approaches offload the intellectual property burden to the open-source community, resulting in absolute firmware transparency at the cost of increased integration effort or hardware bulk.

## Firmware Integrity and Capability Analysis

Exploding the technical realities of each platform requires a deep dive into vendor documentation, software licensing models, and community repositories. The following subsections detail the evidence surrounding the integrity claims for each candidate.

### Texas Instruments CC13xx Family (CC1310 / CC1312R7 / CC1352P)
Texas Instruments (TI) offers a highly capable Sub-1 GHz ecosystem, but it fundamentally conflicts with the requirement for open-source or fully auditable firmware. TI provides a "free-of-charge and royalty-free" wM-Bus OMSv3.0.1 and v4.1.2 compatible software stack for the CC1310 and CC1350 [cite: 17, 18]. This stack natively supports a "Serial" or "Network Processor" mode, which delivers exactly the requested transparent raw-frame output over a 115.2 kbps UART (Universal Asynchronous Receiver-Transmitter) interface to a host MCU [cite: 17, 19].

However, the integrity evidence strictly disqualifies this route. Official TI documentation explicitly states that the installer package contains "portions of object code and API code" [cite: 17]. When questioned directly regarding source availability, TI representatives confirmed: "The wmbus stack is not available in source code, and it is not supported for the CC1312 either, just for the CC1310 and CC1350" [cite: 20]. The stack relies on software developed by StackForce, whose licensing model delivers the protocol stack "as object code and cannot be directly modified," restricting source access solely to the hardware abstraction layer (HAL) [cite: 21]. Therefore, the TI CC13xx ecosystem forces reliance on an opaque, unsigned binary blob, directly violating the primary integrity constraint.

### STMicroelectronics STM32WL55 / WLE5 + STM32Cube
The STM32WL55 and WLE5 devices are highly integrated, dual-core (Cortex-M4 and Cortex-M0+) sub-GHz SoCs that represent the pinnacle of modern hardware security [cite: 22, 23]. At the hardware level, they fully satisfy the "marginally acceptable" integrity requirement (b). The devices feature "secure sub-GHz MAC layer, secure firmware update, secure firmware install and storage and management of secure keys" [cite: 23]. This framework heavily relies on SBSFU (Secure Boot and Secure Firmware Update) protocols alongside TF-M (Trusted Firmware-M) standards to allow a developer to utilize cryptographically signed firmware with verified secure boot.

The software ecosystem, however, presents a significant hurdle. STMicroelectronics relies on its partner, StackForce, to provide the wM-Bus stack. ST representatives officially confirm that "the Wireless M-Bus for the STM32WL is available from our partner StackForce as a commercial version only" [cite: 24]. Because StackForce distributes its OMS wM-Bus stacks as compiled library files (object code) [cite: 21], achieving a working T-mode raw-frame receiver using official libraries means accepting an opaque blob. 

If a developer wishes to leverage the STM32WL55's secure boot capabilities while maintaining total code ownership, they must abandon the StackForce wM-Bus stack and manually implement the wM-Bus PHY (including 3-of-6 decoding, de-whitening, and CRC validation) directly on top of the open-source STM32Cube Sub-GHz Radio (SUBG1) HAL drivers. This is a theoretically sound but highly labor-intensive path.

### Silicon Labs EFR32FG2x + RAIL
The EFR32FG23 is a sub-GHz SoC positioned for smart metering and proprietary protocols [cite: 25]. Silicon Labs provides wM-Bus support via its Radio Abstraction Interface Layer (RAIL) [cite: 26, 27]. The RAIL SDK provides built-in PHY configurations specifically for "WMbus T M2O (100k, 3 of 6)" and includes helper components to process packets and decode manufacturer fields [cite: 26]. 

While Silicon Labs provides application examples as source code [cite: 28], the core RAIL library itself is delivered as a pre-compiled binary library designed to abstract the low-level radio hardware. Consequently, a developer cannot audit or independently build the lowest layers of the RF state machine. While EFR32 devices support advanced hardware security (Secure Vault), the opacity of the RAIL library classifies this approach as heavily reliant on opaque binaries, making it a poor fit for maximum integrity.

### RTL-SDR with rtl-wmbus
For fixed, mains-powered gateways where power consumption is not a primary constraint, the Software-Defined Radio approach offers the highest possible firmware integrity. The `rtl-wmbus` project is a "100% open-source software demodulator for Wireless-M-Bus" written in plain C [cite: 29]. It interfaces with RTL2832-based hardware to perform filtering, FSK demodulation, DC offset removal, clock recovery, and packet decoding for modes T1, C1, and S1 directly from the raw I/Q (In-phase and Quadrature) samples [cite: 29].

This architecture perfectly mirrors the desired topology. A Raspberry Pi host interfaces with the RTL-SDR dongle via USB, executing the `rtl-wmbus` C code compiled directly from source [cite: 29, 30]. The software outputs raw decoded frames via standard output pipes (stdout), which can be effortlessly consumed by parsers like `wmbusmeters` or custom JSON/MQTT bridges [cite: 31, 32]. Because the entire signal processing chain from digitized I/Q samples to link-layer frames happens in open-source user-space software, there are zero vendor firmware blobs involved. This approach is completely auditable and fulfills the highest integrity standard.

### CC1101 Transceivers with Open-Source MCU Firmware
If a chip-down, productizable hardware endpoint is required without the bulk of an SDR, combining a Texas Instruments CC1101 transceiver with an external MCU (such as an ESP32 or a secure-boot enabled STM32) is a highly viable path. The CC1101 is an SPI (Serial Peripheral Interface)-driven transceiver that pushes the burden of PHY/MAC handling onto the host MCU [cite: 33, 34].

The open-source community has extensively mapped the wM-Bus protocol onto the CC1101. Repositories such as the ESPHome WMBus component [cite: 35] and standalone ESP32-CC1101 integrations [cite: 33] successfully receive T1-mode packages. These implementations are delivered as 100% open-source C/C++ code [cite: 33, 34]. 

The primary drawback of this approach is the requirement to manually handle strict operational constraints in the physical layer. Specifically, the CC1101 contains a strict 64-byte internal FIFO (First-In, First-Out) buffer limit [cite: 36]. When receiving extended wM-Bus Mode T frames, the MCU must aggressively read the buffer during reception; failure to do so results in a `STATUS_RXFIFO_OVERFLOW` state, corrupting or dropping the packet entirely [cite: 36, 37]. This mirrors the historical difficulties of the CC112x series. However, unlike proprietary SoC blobs, the developer retains absolute ownership and compilation control over the entire RF abstraction layer.

## Ranked Recommendations and Integration Strategy

Based on the strict preference order established by the integrity parameter (Source Code >> Signed Secure Boot >> Reject Opaque Blobs), the candidate platforms are ranked as follows:

**1. rtl-wmbus on RTL-SDR (Maximal Integrity - Recommended for Rapid Deployment)**
This approach is the undisputed winner for a fixed, mains-powered concentrator. It provides complete source code visibility, natively outputs raw CRC-checked hex frames, and requires zero proprietary licensing.
*   **UART Emulation via `socat`**: To emulate the exact hardware behavior of a Würth Metis-II module over UART, developers can utilize `socat`. By executing a command such as `socat -d -d -v pty,rawer,link=/dev/ttyV0 EXEC:rtl_wmbus,pty,rawer`, the standard output of `rtl-wmbus` is piped into a virtual pseudo-terminal (`/dev/ttyV0`). The existing host application can then read this virtual serial port exactly as if it were a physical IC, requiring zero codebase modifications for ingestion [cite: 38, 39].
*   **Reusability:** The existing host application that handles OMS/AES decryption and JSON-MQTT wrapping can remain untouched. 
*   **Effort:** Extremely low. Compiling `rtl-wmbus` on a Raspberry Pi and piping the output takes less than an hour [cite: 30].

**2. CC1101 Transceiver + Secure Boot Host MCU (High Integrity - Recommended for Custom Hardware)**
If the gateway must eventually evolve into a custom printed circuit board (PCB), pairing a CC1101 with an MCU that supports secure boot (e.g., an STM32 series MCU) satisfies both the source-code preference and hardware scalability.
*   **Reusability:** High. By porting existing open-source C/C++ wM-Bus PHY code to the secure host MCU, the system can stream raw UART data exactly mimicking the Würth Metis-II "read" mode.
*   **Effort:** Requires managing SPI communications, handling strict 100 kbps timing, and writing custom low-level C code to implement the physical layer to avoid the 64-byte FIFO buffer crash [cite: 33, 35, 36].

**3. ST STM32WL55 (Marginally Acceptable - High Effort)**
This SoC is ranked third. It provides industry-leading secure boot and signed firmware updates [cite: 23], satisfying condition (b). However, because the official Stackforce wM-Bus libraries are opaque object code [cite: 24], utilizing this chip with high integrity requires writing a custom wM-Bus PHY layer on top of ST's open-source HAL.
*   **Effort:** Very High. Implementing 3-of-6 decoding and de-whitening from scratch on a new sub-GHz core requires significant RF domain expertise.

**4. TI CC13xx and Silicon Labs EFR32FG2x (Rejected)**
Despite excellent RF performance and turnkey "Serial mode" features, both platforms obfuscate their lower-level PHY/MAC operations behind pre-compiled object libraries or opaque HAL binaries (StackForce for TI, RAIL for Silabs) [cite: 17, 21, 26]. They fail the core integrity requirement and must be rejected.

## Hands-On Verification Requirements

While documentation provides a clear architectural path, several operational realities require immediate bench testing:

1.  **SDR RX Sensitivity and Processing Overhead:** While `rtl-wmbus` functions effectively, RTL-SDR dongles are known to have inferior noise figures and dynamic ranges compared to dedicated silicon like the CC1125 or STM32WL55. The gateway's ability to decode the Apator Ultrimis W's 16 mW signal through walls must be empirically verified.
2.  **Dense T-Mode Collision Handling:** T-mode transmits at 100 kbps. In environments with dozens of meters (especially if electricity meters transmit constantly), SDR software demodulators can struggle with concurrent frame processing. The 3-of-6 decoding efficiency under heavy packet collision conditions must be profiled.
3.  **Honeywell V210H Baseline:** Because explicit transmit frequency and mode configurations for the Honeywell V210H could not be extracted from the primary source data, its adherence to standard T1 or C1 behavior at 868 MHz must be actively verified using an SDR spectrum waterfall before committing to a specific transceiver hardware pipeline.

## Conclusion

The pursuit of a high-integrity, sub-GHz Wireless M-Bus gateway fundamentally clashes with the modern semiconductor industry's approach to RF intellectual property. It is established that major vendors—specifically Texas Instruments and STMicroelectronics—rely heavily on commercial partners like StackForce to provide wM-Bus stacks, which are exclusively distributed as opaque object code or pre-compiled libraries. This reality immediately invalidates turnkey SoC solutions under strict open-source or auditable firmware requirements.

What remains highly viable is the separation of the RF physical layer from the protocol logic. For a mains-powered gateway, the `rtl-wmbus` software running alongside a standard RTL-SDR dongle provides an immediate, 100% open-source bridge from 868 MHz radio waves to decoded link-layer APDUs. For a custom hardware approach, legacy transceivers like the CC1101, mated to a secure-boot capable microcontroller running community-vetted open-source C++ PHY implementations, offer the most balanced compromise between RF stability and firmware sovereignty. Further hands-on research should immediately prioritize testing the sensitivity and collision-handling limits of the RTL-SDR approach against the Apator, Kamstrup, and Honeywell meters in a live RF environment.

**Sources:**
1. [elabuelodelriego.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGH2XVn7bhmZmbPEdk5bXrJ4iofl4xp-R9KDHQKWlQLsrGTKhcV_88VcwJq85b5xFQgiGsUWy-na5uH_dJqh1eET_e4bxPYExvkroHdjUNnuUgaHuXP9O7kqqiK9MDS9OjGHldnqwnssjHKj7J-VuqRoT5L8ie9i2WZQzZCxXRZnklQKz3osbFTL-jhQIe3Zk4I_y6zztHl4rP3uzVBzctbbeumlKZ5CJKK_nZpw7S7CE9Eqg==)
2. [iobroker.net](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEXxIOK7Mv_dN1Ku4ySmBLN01IIH9oC5bixjSjk9PqmZ3OUmRqJNb7UyarF3dVrTbw9Sb6lBjuB5MS38Q6oRqJFl-3MsR6NDA5xGaHTw2aRBUSUjY0syVzEqaRJsQN4Yx6ruLQi70ZkCbQbPtztqtdNON2WL_Zh2CFalX3Q6CUjiUVk)
3. [iobroker.net](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEjVC19zRnEbYa6Wr5JfEumWtBVPKdDUEfkp_uy2QlLoj4R4X4kpQlQYmMrpF4SYfmceo06yzZO-L5Xa_LW_Wh_x8vd4zX-fA2VV20aERrCn_HInJ4mqUlWPCxp2hgv6aJ8pPLGrRyLNzoqZJbKqgZp4P74gduLEYSQcliYo_SGxt1IzkBk)
4. [kamstrup.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQELY9Ib-L_osbvujPlUh8MyVuCfhv6gMmxHFjJ-scYdG38QYLfGSxj8Ld3825FnPu3b1nfRQkxDaDQh6J3I22qn03CZSyaVzyWR8reuAx4q_puERPB09w4W_OO4gSTBBn5djR2h-6u9f1nNdXdx-TnG2u6OlnXhYL5HVuCp)
5. [thegreenify.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQE8eC9y950PmYV8nhg29Mc0r7E7hTBYmF9i7iMU6Hsx3jT5JD65ZeJxVR7cldjrFjrIgqpxTnDieZ6ubpTIBGl4COc6O4DmwTSKsACzbN9TAearx2tf6UepoTb-n3162qNzf82FFOpGBmTZeyAy7ciUFP60qH6cx0jNbsvaXYctCdnqxEwms5AYQnWCu3nfngsHRwHYwM0IiA_4oo7XLy4qkjH31xXwK8W3)
6. [readthedocs.io](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEqrZ1rt9jxdmiMYXr1z0EiIDX16MO4CjsuBuoSTrhJ6UVqw3i6L61IiCKCJgSY4b7xBHs_K2GBUdVyQPs9h8aIzcAUs5doA6DMz7VV6Si9UdDVI2iryqTbXpoBRauRmDdYC5HTbL8xOyDvWI3OJLn00N79QNcq)
7. [readthedocs.io](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHl2yGUFElnVwSx_L8GvatXVBfnFy2Lm2JPcRt0SUo5BoP1YYkQK-FAiuO3T-RoGWvdc8W24a4VPavIsma8Mo6aywC0sBvO6Hr3HrFVNIXE5GbJkYofG3UMr9h3QdjW6hP3CHWpoMEhlxWJrkBOYrW7vg==)
8. [github.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFixxNV7Amh7Ib0ElDSZUFoN0XB9qQ2jYaR4EWoenmouxlCOZ4FAD-Gatg070VybHtFpwq_CqUeXePD2xaAt9PSOWLzPG6H9I8UCcoxH3Jg6JGyxB0y5Dfx4BFbh2z7Z73yDnwFZxOZMv0=)
9. [arturhome.pl](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFR9FtyGRVoLdgcTkLIGanzc_6xdkBTzvzn3B4H6xD44nzHrF7LeWzVeCQR9QGdpkVlEnDXPaPB-vQSZ3N8EblA6X8ZGCBR3SQqvjMnxbAz9cSZyV32joeN7L-CQ2YiVpt-mpNkYZ9rQjKFLEmWtCdBMHRvf-k=)
10. [radioreference.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQE6oWCODx06u87Ms-e4FtrwmzDtzCdvhbsLIkCXH6-c6pWQj-y3Uod2AzqO1ae72_y8vGLluQQq1X-aMiOXFx2hJFvGEuFR9aA_vXjNJuysacBl1nrIDln5plx_R96lFoeYVB-FLAPqHJUsGtN9iEj6NT3S_vrGtMtkFXzE1Niv7jc=)
11. [aliexpress.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEAUgFDfVvXAwIntk-vQPRwYS33P1WSgdeBv0-d18UjJvUj85zGOlnwDCcfn2vlN31nvW3NOujEg6_aLgApd9djIKfLmRtOX4wznHVMz1CF1EMO9A9kLmVpkC7Pa4kmYYz44v_J79rWcO3I)
12. [mouser.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHuvaS2HPUkvR2nYfV0zN1eWePS91418vwDR2JVJWbPnJteihnJkCfwjDL3IZnL_TEXnCdsiwNV5jT8JwTPoh4oTfHllNCNT32rIZ1Jo_bo_k3bZAIPiEmYwLqQxYfDtyRBzwj4GgDp0wckCi72k9EiJ95Lii_3Xa8faBwNnfMx49pQeAmotVtfnS41yWrWYViNSN6SAIhKQWOkFZ6oAL2WzlA3a7J_BQV1aC1WCBeypw==)
13. [digikey.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGoOB7wuMf7x18eSQ7lFqlb1oQxR1tn85NB6fOSwqX2Cf6A4Ek4IwOn2d5AFHjEBdIM1g4h0RmlOOimgJGBPrqzA7hollvrqKceD8dBannZPocsMx0s4oKqT4zU7CT5U9YAnm3rL2Skhk_Rm4RHrW2ieU2DpHstuYg33nDlb0cnIrEC37dkXVOnFQ7LgKG0Ruzt6tubtxclcYNvDYIQ1yxcHvWxGlsvXg==)
14. [digikey.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEHPyWzpe9mClA-xyptEIBtP35XnMC2grOG2wl9oIlIkRHqhOrOS-0EnyQzLoOrRj0iVSkAjWKr2JHvkI8-2ejr0l6xRg08gMD9H2LnY7BBrZYE_N_CqyEnJ6hPkEHq1Sb_Y02ooWF_ktxYhKQsB1HyWD9Wf7abK2Nnw7EmNE3TPp3Il2xS3YLf8Q==)
15. [mouser.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHTa2x6IY4WLftpH52JzOL2VfO19_AVG_FGMYRmh1mkrfSnSIhYtee42MIkgd77PDzYGqluNDuvsdN6VRZIngH1myaYZ5R_03umdCEJJ9xWx9WSv-utr-eSmK-2eAkLj8tMPz0Gm4EhaRLgM3mfjMX6z9MwcsIrqm4gCmqm5DEtcm2_e1zNBZIw74bLhT_X4yTQ_D7YKHipIEAh5V-w7Q==)
16. [mouser.in](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFByUI9yRw4NgA3xRrcjHvP-7y1tW35h51hmOI8JkXArommQpyO4I77YokvpNiiAGhL7kU6GBjW9R0SJ_Eyc9AqfeVglMFdC9BWhQ6eEHe8KP_k5h1TkswSDpZzugggx57hEeYIZX4ZgJRZiuSUNBeym7ahnD1TLLSJXH-0_p4V-H3y6gMiA7lENY25gYUKeWw7VOZRyBh93ozaGdWcxaY3aquUjcLzWD3wQttj16F8Y_IejEIX_740CA==)
17. [device.report](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFlXlZ8uFkM2wwXCUActUtHM7j_OU6fk2f_dWDErufQiFijGUU0ESPobUnn6KVmTndpXpKVu-v5WaqJqsnTWkbPwn9DJQFMs1Rd_bL1xatBAjKgVqMgsfSnhcg7jgT323Z60ZwuQ6aLdqiOJGKxKLtyKrh5V9MDrbuRkYjdB4jSn7S7RHKN3f3AqltnN6bPUOpe)
18. [ti.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGsYPJVvFhbX1XZtPBqsA2GaJ4lcHPtoiHC03PuQZEDT_u0fID0mq2UFSyI7R0iiW0Plh0up5jzjowubzNYRszZSnsMdYTlVlhr5_qqBDeukx504-YgCZ4=)
19. [ti.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQG87nzYrOHpWtuWzuIlQQ0H6HntUjYK2VtWP1aN05yVcjIO9_0OpMcXEIChrtyeEyyj65DF8E8IndHyyzcxvU0P5ad_q8ttrlqCGFPQ6hSfKVSy)
20. [ti.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHNFNGZgkIBqZqFx6BFdpnO7dd-nxLkJT2XXBuTZs556431fgzRMehS5lopVXMLa8Mw4e7xTOPQq8LPgDMneH3PxjnWV4zY1vDU86RA6RqkhyX1lYh1O3hWLjuiuwtzRUC2UIMGCQUXBIl2i4BrVNGt51rVBSRV3qVrd_0A2CySYEOAqwCRciiE1bzF9-n6HItqwfxxfokrjlJBIroJFUbwYH9pdDrjj2MRioF8c5kTPgjmXfzidUBMkoB118Q=)
21. [stackforce.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEGiH7jhyH28Z4pW_SqOeeQyFikTiXmq7PqOtQiK1gppp13kxHNLUQzHu9dSkJUHCytGyq1ruXu-AXaOccOCN82kEcXYsPn_QztQl2PWO6rZ-6i0tOt1IeAQmvKXgxGJRnLbfnocaQB2P95dL69eNByjW2KJw==)
22. [mbed.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGtdhbDyz-8LcbA8nBnsLfOXhjhZkyWWDu8ko4oYoF1GRwSKY9Puh8jio2CVyL_HTWtcDsgvLGYCDuHHO_4qbpXeGEVztTVn7SW0NcJnkfdcxx6Ti0rzEfOc6xT4UUSlyjylh1M)
23. [st.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFAZvTVK4_mqZFuBqLA25jZhS-razbi9tWVR-rvDz4tCyxd6mSK--KhLqL6gSlvi5P-XCk-jMDOrectOUHYnEQGUDbuBwYqLmuoP0XGqd1qxbEaqlgTtjRcpB-iNHdcJNN38AgTBjMrFSknt1ec7Za3FvGGtBw-Gonw88eE)
24. [st.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGltdoNBpXz2ZpzC3Wqa2b6aPOlBAgsiQHHMNVSojs33Vz6qFefpR1W_rWTNGjlA0EzdK8G1ZM41BNMlMMB7elzFSbZioQxYgaMXbNyAuBIjkGWAPpaTdX5Dcwb3T46rMKKlXNFacIMz4TaKoe5GHxn5ghHHztqgx0UeS_uWKJR_Hs-CUAOtZpxpuLznQdcK90=)
25. [silabs.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFmkU01rmo1aulY66b8xOby80HUcZ1mt1k6RuzzgozbGCDODn6rpBPzC9rvjmEjc6toDxXyldANxC-d6-HTclcknL-fXhOxcpevD0_9FL8FjjFmecgXosd-OdfsqX2xfHM=)
26. [silabs.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQF3OWV6xIEKaoWCEeLnLS26MiV88qCoRce_deYz5jFFlVr4vbH19hWyV7EfufxdDzP9oYQy5QLuAIZQaWOCnBniHnBJB-7Ez0j0RgN-853Ba0NI4wrEAi895vifByQJSnZwwSVepgZSoiG0IQhOt9_56PgmkaFyFdTDytG1q8dnIn4wIsM=)
27. [manuals.plus](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHjR1xQFZspZwBX2sBnD4J1brwMcZlAuJjnASlxGTgCcW3E_l4UZIipfMFUBMDR7PfQh4YDeKYiPioXLI6BmUCut80i-P-oyiUAkd0uOUXB8gTxq4GswcCzvv16YQjOvIVLECD0k1lvyH62alCOmphPlq0f0TGhDY5W_yUuQ-CI8pdEfZISzGR0WAiA2o1xtOQ=)
28. [youtube.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEdTfi8b-wJfjbok5APPl2M1Z4wXoUav7NL7lmzRwilnwM535R_JvKasWYror8LYmOANq_39Fpz3FBnm2c4ClmEA3IJ9senu7_6bmgdVXfq7m3FLh-RKxwEKm3KVhu3oB8=)
29. [github.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGjh-i8lbW-AdBW22VFsLvYOUWtEc4wPTX-Ea8IhYGHMTZ72bHLzhibHyq0VkZLRJLBOVLTIxJYc7VT9Q5f-1Vi6cknfOs82tMNWZg8_3IJ44qttsDOXHJGsHkJ)
30. [github.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGjVnuSOZ3KwjMiAxEs99ieQtUe6bntrVHwfJeWib1zKh5LyK5MZddBfNXOn8OJWlgAhrqUB9DJhXkJwkTeKpd4kJC26fYGrdjxWznPJHRzEurhOiC6daBF4MpgxSEihOv8wxaX_tenPGUYZJkxk4-AdyiXmJFVaWjpWbQo7dtyNQ==)
31. [medium.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQH1oFXIZ5UbkQlCxkP5BUqHtpVOfDuYKezRhHWKGtL4iMC4dAZ0SNCZc4ke1gc7AcP6_4Lwr1Zjkl24p6HynZnLryU_i2m1sXIIAvHfmcRHdS18mjpSyN_gqXRMp05qXnse2-wAoTPN3stm37eYQoRUOqlW5bUNaZh2FzcUGuSzJvIYSC0XpuKAT1kJi346sZl-l30=)
32. [github.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQF9VSKnF-JcjLZgrFoEb7zYCRZnftCW89U3YEeuG9lXfGytnJAMazg6fJ4QIzv0wMctsvmhhehfyyQs2IMbYAbYKqYj-saBnZY47p0MM6DrAq9uvuyKJtI46HwyVGYLpw==)
33. [github.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGKATwh5r6dSppwzjUffZiWPfMM_RCoVKIhXhcxAhtOedzUMOcKWuQWYR1t45neYPBrbUbwfoyRF4cHukKkNcQLLV1ipivbJ6mGOolSAWwaBx_r99OqTAx9g0lmt81lccTWSsveKd7x)
34. [github.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFkEaj7e8a5y7_EId4MED6Tw5F6fSs5D34kITK2aVABJb7RDSw10Annxun_9HobzXVsVaVpoco3tgdcWCMt0T-QO0tAOzIgexLooeyz9RwI-s6qpqwZgshZgM9nSMXf)
35. [github.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFKBODw6qei-G2QuBPRPCqc9B8t2MeX8pdkebJ0hyHJIOJRHswFxu9NMS3L_2oguCNAcw1ppBgp6LbJzsRjNGvJ0gAo0Wjvec1rEJFl0Y5p7rWHYrAJ9l_hfeM8f1-rq2vwPKs8s04r)
36. [github.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGtUjO-1yDNAiGdUu5DSbUq5SOsqIPefxfhcZStq8t4DThOHaCSHc8J3HmLmXBH-OXmIJnGXj5d-M2nxlvl1cnV_UgkbEKXP2fUGfdCYHXNojIA2Y6hQjYFz32YhlNOKGAEuGAHCRvUOsK9d2LuFuC69i0x)
37. [github.com](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQF7f0_kG9dT9YRRahgjpXrzutR3mju78yjkk7hYWEB6-tuSyLdloIkItySQjSCO5qWguNEiBUGtLlwN4eNnJvDTTAvN4toVH_U05Wv3eHu644i_6g2b)
38. [marcusfolkesson.se](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGW7iXOrvo57MeYhz4MB_D2fUlGMKzYLDyjPXUQM06UK59-Y3Zi7j8vMmxSmM76BtO0CbOANQocuv1TJ2kY8c8oYYiKEMPgDx9WG9qbaJi6YOFkc4QSUJtEuTx97J-acxp9d2WwFD7DGnEhYlsAONYFUYvzh-W6u-OxZQ==)
39. [jamesthom.as](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEuTuBvxYjADeLLPBa-sJ1dBotDjagMOroXB7k0MDp6Rxelz-cgtjahcRmP0PPwhMvBVAHOE-CxlkntMiWvzk70ucn-9tPQViXi2sSWY2ZV9ivDUKN4PunQy38cmywo2Zbh-4TIBqrn43QMnsm5zNqOxDRI)

## Sources

1. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGH2XVn7bhmZmbPEdk5bXrJ4iofl4xp-R9KDHQKWlQLsrGTKhcV_88VcwJq85b5xFQgiGsUWy-na5uH_dJqh1eET_e4bxPYExvkroHdjUNnuUgaHuXP9O7kqqiK9MDS9OjGHldnqwnssjHKj7J-VuqRoT5L8ie9i2WZQzZCxXRZnklQKz3osbFTL-jhQIe3Zk4I_y6zztHl4rP3uzVBzctbbeumlKZ5CJKK_nZpw7S7CE9Eqg==](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGH2XVn7bhmZmbPEdk5bXrJ4iofl4xp-R9KDHQKWlQLsrGTKhcV_88VcwJq85b5xFQgiGsUWy-na5uH_dJqh1eET_e4bxPYExvkroHdjUNnuUgaHuXP9O7kqqiK9MDS9OjGHldnqwnssjHKj7J-VuqRoT5L8ie9i2WZQzZCxXRZnklQKz3osbFTL-jhQIe3Zk4I_y6zztHl4rP3uzVBzctbbeumlKZ5CJKK_nZpw7S7CE9Eqg==)
2. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEXxIOK7Mv_dN1Ku4ySmBLN01IIH9oC5bixjSjk9PqmZ3OUmRqJNb7UyarF3dVrTbw9Sb6lBjuB5MS38Q6oRqJFl-3MsR6NDA5xGaHTw2aRBUSUjY0syVzEqaRJsQN4Yx6ruLQi70ZkCbQbPtztqtdNON2WL_Zh2CFalX3Q6CUjiUVk](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEXxIOK7Mv_dN1Ku4ySmBLN01IIH9oC5bixjSjk9PqmZ3OUmRqJNb7UyarF3dVrTbw9Sb6lBjuB5MS38Q6oRqJFl-3MsR6NDA5xGaHTw2aRBUSUjY0syVzEqaRJsQN4Yx6ruLQi70ZkCbQbPtztqtdNON2WL_Zh2CFalX3Q6CUjiUVk)
3. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEjVC19zRnEbYa6Wr5JfEumWtBVPKdDUEfkp_uy2QlLoj4R4X4kpQlQYmMrpF4SYfmceo06yzZO-L5Xa_LW_Wh_x8vd4zX-fA2VV20aERrCn_HInJ4mqUlWPCxp2hgv6aJ8pPLGrRyLNzoqZJbKqgZp4P74gduLEYSQcliYo_SGxt1IzkBk](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEjVC19zRnEbYa6Wr5JfEumWtBVPKdDUEfkp_uy2QlLoj4R4X4kpQlQYmMrpF4SYfmceo06yzZO-L5Xa_LW_Wh_x8vd4zX-fA2VV20aERrCn_HInJ4mqUlWPCxp2hgv6aJ8pPLGrRyLNzoqZJbKqgZp4P74gduLEYSQcliYo_SGxt1IzkBk)
4. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQE8eC9y950PmYV8nhg29Mc0r7E7hTBYmF9i7iMU6Hsx3jT5JD65ZeJxVR7cldjrFjrIgqpxTnDieZ6ubpTIBGl4COc6O4DmwTSKsACzbN9TAearx2tf6UepoTb-n3162qNzf82FFOpGBmTZeyAy7ciUFP60qH6cx0jNbsvaXYctCdnqxEwms5AYQnWCu3nfngsHRwHYwM0IiA_4oo7XLy4qkjH31xXwK8W3](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQE8eC9y950PmYV8nhg29Mc0r7E7hTBYmF9i7iMU6Hsx3jT5JD65ZeJxVR7cldjrFjrIgqpxTnDieZ6ubpTIBGl4COc6O4DmwTSKsACzbN9TAearx2tf6UepoTb-n3162qNzf82FFOpGBmTZeyAy7ciUFP60qH6cx0jNbsvaXYctCdnqxEwms5AYQnWCu3nfngsHRwHYwM0IiA_4oo7XLy4qkjH31xXwK8W3)
5. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQELY9Ib-L_osbvujPlUh8MyVuCfhv6gMmxHFjJ-scYdG38QYLfGSxj8Ld3825FnPu3b1nfRQkxDaDQh6J3I22qn03CZSyaVzyWR8reuAx4q_puERPB09w4W_OO4gSTBBn5djR2h-6u9f1nNdXdx-TnG2u6OlnXhYL5HVuCp](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQELY9Ib-L_osbvujPlUh8MyVuCfhv6gMmxHFjJ-scYdG38QYLfGSxj8Ld3825FnPu3b1nfRQkxDaDQh6J3I22qn03CZSyaVzyWR8reuAx4q_puERPB09w4W_OO4gSTBBn5djR2h-6u9f1nNdXdx-TnG2u6OlnXhYL5HVuCp)
6. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEqrZ1rt9jxdmiMYXr1z0EiIDX16MO4CjsuBuoSTrhJ6UVqw3i6L61IiCKCJgSY4b7xBHs_K2GBUdVyQPs9h8aIzcAUs5doA6DMz7VV6Si9UdDVI2iryqTbXpoBRauRmDdYC5HTbL8xOyDvWI3OJLn00N79QNcq](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEqrZ1rt9jxdmiMYXr1z0EiIDX16MO4CjsuBuoSTrhJ6UVqw3i6L61IiCKCJgSY4b7xBHs_K2GBUdVyQPs9h8aIzcAUs5doA6DMz7VV6Si9UdDVI2iryqTbXpoBRauRmDdYC5HTbL8xOyDvWI3OJLn00N79QNcq)
7. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHl2yGUFElnVwSx_L8GvatXVBfnFy2Lm2JPcRt0SUo5BoP1YYkQK-FAiuO3T-RoGWvdc8W24a4VPavIsma8Mo6aywC0sBvO6Hr3HrFVNIXE5GbJkYofG3UMr9h3QdjW6hP3CHWpoMEhlxWJrkBOYrW7vg==](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHl2yGUFElnVwSx_L8GvatXVBfnFy2Lm2JPcRt0SUo5BoP1YYkQK-FAiuO3T-RoGWvdc8W24a4VPavIsma8Mo6aywC0sBvO6Hr3HrFVNIXE5GbJkYofG3UMr9h3QdjW6hP3CHWpoMEhlxWJrkBOYrW7vg==)
8. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFR9FtyGRVoLdgcTkLIGanzc_6xdkBTzvzn3B4H6xD44nzHrF7LeWzVeCQR9QGdpkVlEnDXPaPB-vQSZ3N8EblA6X8ZGCBR3SQqvjMnxbAz9cSZyV32joeN7L-CQ2YiVpt-mpNkYZ9rQjKFLEmWtCdBMHRvf-k=](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFR9FtyGRVoLdgcTkLIGanzc_6xdkBTzvzn3B4H6xD44nzHrF7LeWzVeCQR9QGdpkVlEnDXPaPB-vQSZ3N8EblA6X8ZGCBR3SQqvjMnxbAz9cSZyV32joeN7L-CQ2YiVpt-mpNkYZ9rQjKFLEmWtCdBMHRvf-k=)
9. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFixxNV7Amh7Ib0ElDSZUFoN0XB9qQ2jYaR4EWoenmouxlCOZ4FAD-Gatg070VybHtFpwq_CqUeXePD2xaAt9PSOWLzPG6H9I8UCcoxH3Jg6JGyxB0y5Dfx4BFbh2z7Z73yDnwFZxOZMv0=](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFixxNV7Amh7Ib0ElDSZUFoN0XB9qQ2jYaR4EWoenmouxlCOZ4FAD-Gatg070VybHtFpwq_CqUeXePD2xaAt9PSOWLzPG6H9I8UCcoxH3Jg6JGyxB0y5Dfx4BFbh2z7Z73yDnwFZxOZMv0=)
10. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQE6oWCODx06u87Ms-e4FtrwmzDtzCdvhbsLIkCXH6-c6pWQj-y3Uod2AzqO1ae72_y8vGLluQQq1X-aMiOXFx2hJFvGEuFR9aA_vXjNJuysacBl1nrIDln5plx_R96lFoeYVB-FLAPqHJUsGtN9iEj6NT3S_vrGtMtkFXzE1Niv7jc=](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQE6oWCODx06u87Ms-e4FtrwmzDtzCdvhbsLIkCXH6-c6pWQj-y3Uod2AzqO1ae72_y8vGLluQQq1X-aMiOXFx2hJFvGEuFR9aA_vXjNJuysacBl1nrIDln5plx_R96lFoeYVB-FLAPqHJUsGtN9iEj6NT3S_vrGtMtkFXzE1Niv7jc=)
11. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEAUgFDfVvXAwIntk-vQPRwYS33P1WSgdeBv0-d18UjJvUj85zGOlnwDCcfn2vlN31nvW3NOujEg6_aLgApd9djIKfLmRtOX4wznHVMz1CF1EMO9A9kLmVpkC7Pa4kmYYz44v_J79rWcO3I](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEAUgFDfVvXAwIntk-vQPRwYS33P1WSgdeBv0-d18UjJvUj85zGOlnwDCcfn2vlN31nvW3NOujEg6_aLgApd9djIKfLmRtOX4wznHVMz1CF1EMO9A9kLmVpkC7Pa4kmYYz44v_J79rWcO3I)
12. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHuvaS2HPUkvR2nYfV0zN1eWePS91418vwDR2JVJWbPnJteihnJkCfwjDL3IZnL_TEXnCdsiwNV5jT8JwTPoh4oTfHllNCNT32rIZ1Jo_bo_k3bZAIPiEmYwLqQxYfDtyRBzwj4GgDp0wckCi72k9EiJ95Lii_3Xa8faBwNnfMx49pQeAmotVtfnS41yWrWYViNSN6SAIhKQWOkFZ6oAL2WzlA3a7J_BQV1aC1WCBeypw==](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHuvaS2HPUkvR2nYfV0zN1eWePS91418vwDR2JVJWbPnJteihnJkCfwjDL3IZnL_TEXnCdsiwNV5jT8JwTPoh4oTfHllNCNT32rIZ1Jo_bo_k3bZAIPiEmYwLqQxYfDtyRBzwj4GgDp0wckCi72k9EiJ95Lii_3Xa8faBwNnfMx49pQeAmotVtfnS41yWrWYViNSN6SAIhKQWOkFZ6oAL2WzlA3a7J_BQV1aC1WCBeypw==)
13. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEHPyWzpe9mClA-xyptEIBtP35XnMC2grOG2wl9oIlIkRHqhOrOS-0EnyQzLoOrRj0iVSkAjWKr2JHvkI8-2ejr0l6xRg08gMD9H2LnY7BBrZYE_N_CqyEnJ6hPkEHq1Sb_Y02ooWF_ktxYhKQsB1HyWD9Wf7abK2Nnw7EmNE3TPp3Il2xS3YLf8Q==](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEHPyWzpe9mClA-xyptEIBtP35XnMC2grOG2wl9oIlIkRHqhOrOS-0EnyQzLoOrRj0iVSkAjWKr2JHvkI8-2ejr0l6xRg08gMD9H2LnY7BBrZYE_N_CqyEnJ6hPkEHq1Sb_Y02ooWF_ktxYhKQsB1HyWD9Wf7abK2Nnw7EmNE3TPp3Il2xS3YLf8Q==)
14. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGoOB7wuMf7x18eSQ7lFqlb1oQxR1tn85NB6fOSwqX2Cf6A4Ek4IwOn2d5AFHjEBdIM1g4h0RmlOOimgJGBPrqzA7hollvrqKceD8dBannZPocsMx0s4oKqT4zU7CT5U9YAnm3rL2Skhk_Rm4RHrW2ieU2DpHstuYg33nDlb0cnIrEC37dkXVOnFQ7LgKG0Ruzt6tubtxclcYNvDYIQ1yxcHvWxGlsvXg==](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGoOB7wuMf7x18eSQ7lFqlb1oQxR1tn85NB6fOSwqX2Cf6A4Ek4IwOn2d5AFHjEBdIM1g4h0RmlOOimgJGBPrqzA7hollvrqKceD8dBannZPocsMx0s4oKqT4zU7CT5U9YAnm3rL2Skhk_Rm4RHrW2ieU2DpHstuYg33nDlb0cnIrEC37dkXVOnFQ7LgKG0Ruzt6tubtxclcYNvDYIQ1yxcHvWxGlsvXg==)
15. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHTa2x6IY4WLftpH52JzOL2VfO19_AVG_FGMYRmh1mkrfSnSIhYtee42MIkgd77PDzYGqluNDuvsdN6VRZIngH1myaYZ5R_03umdCEJJ9xWx9WSv-utr-eSmK-2eAkLj8tMPz0Gm4EhaRLgM3mfjMX6z9MwcsIrqm4gCmqm5DEtcm2_e1zNBZIw74bLhT_X4yTQ_D7YKHipIEAh5V-w7Q==](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHTa2x6IY4WLftpH52JzOL2VfO19_AVG_FGMYRmh1mkrfSnSIhYtee42MIkgd77PDzYGqluNDuvsdN6VRZIngH1myaYZ5R_03umdCEJJ9xWx9WSv-utr-eSmK-2eAkLj8tMPz0Gm4EhaRLgM3mfjMX6z9MwcsIrqm4gCmqm5DEtcm2_e1zNBZIw74bLhT_X4yTQ_D7YKHipIEAh5V-w7Q==)
16. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFByUI9yRw4NgA3xRrcjHvP-7y1tW35h51hmOI8JkXArommQpyO4I77YokvpNiiAGhL7kU6GBjW9R0SJ_Eyc9AqfeVglMFdC9BWhQ6eEHe8KP_k5h1TkswSDpZzugggx57hEeYIZX4ZgJRZiuSUNBeym7ahnD1TLLSJXH-0_p4V-H3y6gMiA7lENY25gYUKeWw7VOZRyBh93ozaGdWcxaY3aquUjcLzWD3wQttj16F8Y_IejEIX_740CA==](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFByUI9yRw4NgA3xRrcjHvP-7y1tW35h51hmOI8JkXArommQpyO4I77YokvpNiiAGhL7kU6GBjW9R0SJ_Eyc9AqfeVglMFdC9BWhQ6eEHe8KP_k5h1TkswSDpZzugggx57hEeYIZX4ZgJRZiuSUNBeym7ahnD1TLLSJXH-0_p4V-H3y6gMiA7lENY25gYUKeWw7VOZRyBh93ozaGdWcxaY3aquUjcLzWD3wQttj16F8Y_IejEIX_740CA==)
17. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFlXlZ8uFkM2wwXCUActUtHM7j_OU6fk2f_dWDErufQiFijGUU0ESPobUnn6KVmTndpXpKVu-v5WaqJqsnTWkbPwn9DJQFMs1Rd_bL1xatBAjKgVqMgsfSnhcg7jgT323Z60ZwuQ6aLdqiOJGKxKLtyKrh5V9MDrbuRkYjdB4jSn7S7RHKN3f3AqltnN6bPUOpe](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFlXlZ8uFkM2wwXCUActUtHM7j_OU6fk2f_dWDErufQiFijGUU0ESPobUnn6KVmTndpXpKVu-v5WaqJqsnTWkbPwn9DJQFMs1Rd_bL1xatBAjKgVqMgsfSnhcg7jgT323Z60ZwuQ6aLdqiOJGKxKLtyKrh5V9MDrbuRkYjdB4jSn7S7RHKN3f3AqltnN6bPUOpe)
18. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGsYPJVvFhbX1XZtPBqsA2GaJ4lcHPtoiHC03PuQZEDT_u0fID0mq2UFSyI7R0iiW0Plh0up5jzjowubzNYRszZSnsMdYTlVlhr5_qqBDeukx504-YgCZ4=](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGsYPJVvFhbX1XZtPBqsA2GaJ4lcHPtoiHC03PuQZEDT_u0fID0mq2UFSyI7R0iiW0Plh0up5jzjowubzNYRszZSnsMdYTlVlhr5_qqBDeukx504-YgCZ4=)
19. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQG87nzYrOHpWtuWzuIlQQ0H6HntUjYK2VtWP1aN05yVcjIO9_0OpMcXEIChrtyeEyyj65DF8E8IndHyyzcxvU0P5ad_q8ttrlqCGFPQ6hSfKVSy](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQG87nzYrOHpWtuWzuIlQQ0H6HntUjYK2VtWP1aN05yVcjIO9_0OpMcXEIChrtyeEyyj65DF8E8IndHyyzcxvU0P5ad_q8ttrlqCGFPQ6hSfKVSy)
20. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHNFNGZgkIBqZqFx6BFdpnO7dd-nxLkJT2XXBuTZs556431fgzRMehS5lopVXMLa8Mw4e7xTOPQq8LPgDMneH3PxjnWV4zY1vDU86RA6RqkhyX1lYh1O3hWLjuiuwtzRUC2UIMGCQUXBIl2i4BrVNGt51rVBSRV3qVrd_0A2CySYEOAqwCRciiE1bzF9-n6HItqwfxxfokrjlJBIroJFUbwYH9pdDrjj2MRioF8c5kTPgjmXfzidUBMkoB118Q=](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHNFNGZgkIBqZqFx6BFdpnO7dd-nxLkJT2XXBuTZs556431fgzRMehS5lopVXMLa8Mw4e7xTOPQq8LPgDMneH3PxjnWV4zY1vDU86RA6RqkhyX1lYh1O3hWLjuiuwtzRUC2UIMGCQUXBIl2i4BrVNGt51rVBSRV3qVrd_0A2CySYEOAqwCRciiE1bzF9-n6HItqwfxxfokrjlJBIroJFUbwYH9pdDrjj2MRioF8c5kTPgjmXfzidUBMkoB118Q=)
21. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEGiH7jhyH28Z4pW_SqOeeQyFikTiXmq7PqOtQiK1gppp13kxHNLUQzHu9dSkJUHCytGyq1ruXu-AXaOccOCN82kEcXYsPn_QztQl2PWO6rZ-6i0tOt1IeAQmvKXgxGJRnLbfnocaQB2P95dL69eNByjW2KJw==](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEGiH7jhyH28Z4pW_SqOeeQyFikTiXmq7PqOtQiK1gppp13kxHNLUQzHu9dSkJUHCytGyq1ruXu-AXaOccOCN82kEcXYsPn_QztQl2PWO6rZ-6i0tOt1IeAQmvKXgxGJRnLbfnocaQB2P95dL69eNByjW2KJw==)
22. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFAZvTVK4_mqZFuBqLA25jZhS-razbi9tWVR-rvDz4tCyxd6mSK--KhLqL6gSlvi5P-XCk-jMDOrectOUHYnEQGUDbuBwYqLmuoP0XGqd1qxbEaqlgTtjRcpB-iNHdcJNN38AgTBjMrFSknt1ec7Za3FvGGtBw-Gonw88eE](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFAZvTVK4_mqZFuBqLA25jZhS-razbi9tWVR-rvDz4tCyxd6mSK--KhLqL6gSlvi5P-XCk-jMDOrectOUHYnEQGUDbuBwYqLmuoP0XGqd1qxbEaqlgTtjRcpB-iNHdcJNN38AgTBjMrFSknt1ec7Za3FvGGtBw-Gonw88eE)
23. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGtdhbDyz-8LcbA8nBnsLfOXhjhZkyWWDu8ko4oYoF1GRwSKY9Puh8jio2CVyL_HTWtcDsgvLGYCDuHHO_4qbpXeGEVztTVn7SW0NcJnkfdcxx6Ti0rzEfOc6xT4UUSlyjylh1M](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGtdhbDyz-8LcbA8nBnsLfOXhjhZkyWWDu8ko4oYoF1GRwSKY9Puh8jio2CVyL_HTWtcDsgvLGYCDuHHO_4qbpXeGEVztTVn7SW0NcJnkfdcxx6Ti0rzEfOc6xT4UUSlyjylh1M)
24. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGltdoNBpXz2ZpzC3Wqa2b6aPOlBAgsiQHHMNVSojs33Vz6qFefpR1W_rWTNGjlA0EzdK8G1ZM41BNMlMMB7elzFSbZioQxYgaMXbNyAuBIjkGWAPpaTdX5Dcwb3T46rMKKlXNFacIMz4TaKoe5GHxn5ghHHztqgx0UeS_uWKJR_Hs-CUAOtZpxpuLznQdcK90=](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGltdoNBpXz2ZpzC3Wqa2b6aPOlBAgsiQHHMNVSojs33Vz6qFefpR1W_rWTNGjlA0EzdK8G1ZM41BNMlMMB7elzFSbZioQxYgaMXbNyAuBIjkGWAPpaTdX5Dcwb3T46rMKKlXNFacIMz4TaKoe5GHxn5ghHHztqgx0UeS_uWKJR_Hs-CUAOtZpxpuLznQdcK90=)
25. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFmkU01rmo1aulY66b8xOby80HUcZ1mt1k6RuzzgozbGCDODn6rpBPzC9rvjmEjc6toDxXyldANxC-d6-HTclcknL-fXhOxcpevD0_9FL8FjjFmecgXosd-OdfsqX2xfHM=](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFmkU01rmo1aulY66b8xOby80HUcZ1mt1k6RuzzgozbGCDODn6rpBPzC9rvjmEjc6toDxXyldANxC-d6-HTclcknL-fXhOxcpevD0_9FL8FjjFmecgXosd-OdfsqX2xfHM=)
26. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQF3OWV6xIEKaoWCEeLnLS26MiV88qCoRce_deYz5jFFlVr4vbH19hWyV7EfufxdDzP9oYQy5QLuAIZQaWOCnBniHnBJB-7Ez0j0RgN-853Ba0NI4wrEAi895vifByQJSnZwwSVepgZSoiG0IQhOt9_56PgmkaFyFdTDytG1q8dnIn4wIsM=](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQF3OWV6xIEKaoWCEeLnLS26MiV88qCoRce_deYz5jFFlVr4vbH19hWyV7EfufxdDzP9oYQy5QLuAIZQaWOCnBniHnBJB-7Ez0j0RgN-853Ba0NI4wrEAi895vifByQJSnZwwSVepgZSoiG0IQhOt9_56PgmkaFyFdTDytG1q8dnIn4wIsM=)
27. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHjR1xQFZspZwBX2sBnD4J1brwMcZlAuJjnASlxGTgCcW3E_l4UZIipfMFUBMDR7PfQh4YDeKYiPioXLI6BmUCut80i-P-oyiUAkd0uOUXB8gTxq4GswcCzvv16YQjOvIVLECD0k1lvyH62alCOmphPlq0f0TGhDY5W_yUuQ-CI8pdEfZISzGR0WAiA2o1xtOQ=](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQHjR1xQFZspZwBX2sBnD4J1brwMcZlAuJjnASlxGTgCcW3E_l4UZIipfMFUBMDR7PfQh4YDeKYiPioXLI6BmUCut80i-P-oyiUAkd0uOUXB8gTxq4GswcCzvv16YQjOvIVLECD0k1lvyH62alCOmphPlq0f0TGhDY5W_yUuQ-CI8pdEfZISzGR0WAiA2o1xtOQ=)
28. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEdTfi8b-wJfjbok5APPl2M1Z4wXoUav7NL7lmzRwilnwM535R_JvKasWYror8LYmOANq_39Fpz3FBnm2c4ClmEA3IJ9senu7_6bmgdVXfq7m3FLh-RKxwEKm3KVhu3oB8=](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEdTfi8b-wJfjbok5APPl2M1Z4wXoUav7NL7lmzRwilnwM535R_JvKasWYror8LYmOANq_39Fpz3FBnm2c4ClmEA3IJ9senu7_6bmgdVXfq7m3FLh-RKxwEKm3KVhu3oB8=)
29. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGjh-i8lbW-AdBW22VFsLvYOUWtEc4wPTX-Ea8IhYGHMTZ72bHLzhibHyq0VkZLRJLBOVLTIxJYc7VT9Q5f-1Vi6cknfOs82tMNWZg8_3IJ44qttsDOXHJGsHkJ](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGjh-i8lbW-AdBW22VFsLvYOUWtEc4wPTX-Ea8IhYGHMTZ72bHLzhibHyq0VkZLRJLBOVLTIxJYc7VT9Q5f-1Vi6cknfOs82tMNWZg8_3IJ44qttsDOXHJGsHkJ)
30. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGjVnuSOZ3KwjMiAxEs99ieQtUe6bntrVHwfJeWib1zKh5LyK5MZddBfNXOn8OJWlgAhrqUB9DJhXkJwkTeKpd4kJC26fYGrdjxWznPJHRzEurhOiC6daBF4MpgxSEihOv8wxaX_tenPGUYZJkxk4-AdyiXmJFVaWjpWbQo7dtyNQ==](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGjVnuSOZ3KwjMiAxEs99ieQtUe6bntrVHwfJeWib1zKh5LyK5MZddBfNXOn8OJWlgAhrqUB9DJhXkJwkTeKpd4kJC26fYGrdjxWznPJHRzEurhOiC6daBF4MpgxSEihOv8wxaX_tenPGUYZJkxk4-AdyiXmJFVaWjpWbQo7dtyNQ==)
31. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQF9VSKnF-JcjLZgrFoEb7zYCRZnftCW89U3YEeuG9lXfGytnJAMazg6fJ4QIzv0wMctsvmhhehfyyQs2IMbYAbYKqYj-saBnZY47p0MM6DrAq9uvuyKJtI46HwyVGYLpw==](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQF9VSKnF-JcjLZgrFoEb7zYCRZnftCW89U3YEeuG9lXfGytnJAMazg6fJ4QIzv0wMctsvmhhehfyyQs2IMbYAbYKqYj-saBnZY47p0MM6DrAq9uvuyKJtI46HwyVGYLpw==)
32. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQH1oFXIZ5UbkQlCxkP5BUqHtpVOfDuYKezRhHWKGtL4iMC4dAZ0SNCZc4ke1gc7AcP6_4Lwr1Zjkl24p6HynZnLryU_i2m1sXIIAvHfmcRHdS18mjpSyN_gqXRMp05qXnse2-wAoTPN3stm37eYQoRUOqlW5bUNaZh2FzcUGuSzJvIYSC0XpuKAT1kJi346sZl-l30=](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQH1oFXIZ5UbkQlCxkP5BUqHtpVOfDuYKezRhHWKGtL4iMC4dAZ0SNCZc4ke1gc7AcP6_4Lwr1Zjkl24p6HynZnLryU_i2m1sXIIAvHfmcRHdS18mjpSyN_gqXRMp05qXnse2-wAoTPN3stm37eYQoRUOqlW5bUNaZh2FzcUGuSzJvIYSC0XpuKAT1kJi346sZl-l30=)
33. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFkEaj7e8a5y7_EId4MED6Tw5F6fSs5D34kITK2aVABJb7RDSw10Annxun_9HobzXVsVaVpoco3tgdcWCMt0T-QO0tAOzIgexLooeyz9RwI-s6qpqwZgshZgM9nSMXf](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFkEaj7e8a5y7_EId4MED6Tw5F6fSs5D34kITK2aVABJb7RDSw10Annxun_9HobzXVsVaVpoco3tgdcWCMt0T-QO0tAOzIgexLooeyz9RwI-s6qpqwZgshZgM9nSMXf)
34. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGKATwh5r6dSppwzjUffZiWPfMM_RCoVKIhXhcxAhtOedzUMOcKWuQWYR1t45neYPBrbUbwfoyRF4cHukKkNcQLLV1ipivbJ6mGOolSAWwaBx_r99OqTAx9g0lmt81lccTWSsveKd7x](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGKATwh5r6dSppwzjUffZiWPfMM_RCoVKIhXhcxAhtOedzUMOcKWuQWYR1t45neYPBrbUbwfoyRF4cHukKkNcQLLV1ipivbJ6mGOolSAWwaBx_r99OqTAx9g0lmt81lccTWSsveKd7x)
35. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFKBODw6qei-G2QuBPRPCqc9B8t2MeX8pdkebJ0hyHJIOJRHswFxu9NMS3L_2oguCNAcw1ppBgp6LbJzsRjNGvJ0gAo0Wjvec1rEJFl0Y5p7rWHYrAJ9l_hfeM8f1-rq2vwPKs8s04r](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQFKBODw6qei-G2QuBPRPCqc9B8t2MeX8pdkebJ0hyHJIOJRHswFxu9NMS3L_2oguCNAcw1ppBgp6LbJzsRjNGvJ0gAo0Wjvec1rEJFl0Y5p7rWHYrAJ9l_hfeM8f1-rq2vwPKs8s04r)
36. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGtUjO-1yDNAiGdUu5DSbUq5SOsqIPefxfhcZStq8t4DThOHaCSHc8J3HmLmXBH-OXmIJnGXj5d-M2nxlvl1cnV_UgkbEKXP2fUGfdCYHXNojIA2Y6hQjYFz32YhlNOKGAEuGAHCRvUOsK9d2LuFuC69i0x](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGtUjO-1yDNAiGdUu5DSbUq5SOsqIPefxfhcZStq8t4DThOHaCSHc8J3HmLmXBH-OXmIJnGXj5d-M2nxlvl1cnV_UgkbEKXP2fUGfdCYHXNojIA2Y6hQjYFz32YhlNOKGAEuGAHCRvUOsK9d2LuFuC69i0x)
37. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQF7f0_kG9dT9YRRahgjpXrzutR3mju78yjkk7hYWEB6-tuSyLdloIkItySQjSCO5qWguNEiBUGtLlwN4eNnJvDTTAvN4toVH_U05Wv3eHu644i_6g2b](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQF7f0_kG9dT9YRRahgjpXrzutR3mju78yjkk7hYWEB6-tuSyLdloIkItySQjSCO5qWguNEiBUGtLlwN4eNnJvDTTAvN4toVH_U05Wv3eHu644i_6g2b)
38. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEuTuBvxYjADeLLPBa-sJ1dBotDjagMOroXB7k0MDp6Rxelz-cgtjahcRmP0PPwhMvBVAHOE-CxlkntMiWvzk70ucn-9tPQViXi2sSWY2ZV9ivDUKN4PunQy38cmywo2Zbh-4TIBqrn43QMnsm5zNqOxDRI](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQEuTuBvxYjADeLLPBa-sJ1dBotDjagMOroXB7k0MDp6Rxelz-cgtjahcRmP0PPwhMvBVAHOE-CxlkntMiWvzk70ucn-9tPQViXi2sSWY2ZV9ivDUKN4PunQy38cmywo2Zbh-4TIBqrn43QMnsm5zNqOxDRI)
39. [https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGW7iXOrvo57MeYhz4MB_D2fUlGMKzYLDyjPXUQM06UK59-Y3Zi7j8vMmxSmM76BtO0CbOANQocuv1TJ2kY8c8oYYiKEMPgDx9WG9qbaJi6YOFkc4QSUJtEuTx97J-acxp9d2WwFD7DGnEhYlsAONYFUYvzh-W6u-OxZQ==](https://vertexaisearch.cloud.google.com/grounding-api-redirect/AUZIYQGW7iXOrvo57MeYhz4MB_D2fUlGMKzYLDyjPXUQM06UK59-Y3Zi7j8vMmxSmM76BtO0CbOANQocuv1TJ2kY8c8oYYiKEMPgDx9WG9qbaJi6YOFkc4QSUJtEuTx97J-acxp9d2WwFD7DGnEhYlsAONYFUYvzh-W6u-OxZQ==)
