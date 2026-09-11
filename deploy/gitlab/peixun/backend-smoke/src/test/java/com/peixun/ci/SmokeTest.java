package com.peixun.ci;

import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * CI 冒烟：断言执行环境满足 maven-build Profile 的钉子（JDK17+）。
 * 不触达业务代码，只验证 runner→容器→Maven→junit 报告链路。
 */
class SmokeTest {

    @Test
    void runsOnJdk17OrNewer() {
        int feature = Runtime.version().feature();
        assertTrue(feature >= 17, "JDK " + feature + " does not meet the pinned JDK17 baseline");
    }

    @Test
    void utf8SmokeValueIsStable() {
        assertTrue("企业学堂".length() > 0, "UTF-8 source encoding is broken");
    }
}
