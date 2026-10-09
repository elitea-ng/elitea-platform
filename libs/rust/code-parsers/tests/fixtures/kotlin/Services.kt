package com.example.shop.service

import com.example.shop.model.Product
import com.example.shop.model.CheckoutResult
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.withContext

interface Catalogue {
    fun products(): List<Product>
    suspend fun refresh()
}

fun interface PriceRule {
    fun apply(price: Long): Long
}

interface AuditedCatalogue : Catalogue, AutoCloseable

class CachingCatalogue(private val delegate: Catalogue) : Catalogue by delegate {
    private val cache = mutableMapOf<Long, Product>()
    lateinit var lastRefresh: String
    var hits: Int = 0
        private set

    override fun products(): List<Product> {
        hits++
        return cache.values.toList()
    }

    override suspend fun refresh() = withContext(Dispatchers.IO) {
        delegate.refresh()
        cache.clear()
    }

    companion object {
        const val MAX_SIZE = 1_000
        fun create(source: Catalogue): CachingCatalogue = CachingCatalogue(source)
    }

    inner class Stats {
        fun ratio(): Double = hits.toDouble() / MAX_SIZE
    }
}

object CheckoutService : AutoCloseable {
    private val logger = Logger("checkout")

    @Throws(IllegalStateException::class)
    suspend fun checkout(items: List<Product>): CheckoutResult {
        require(items.isNotEmpty())
        val total = items.sumOf { it.price.cents }
        logger.info("total=$total")
        return if (total > 0) CheckoutResult.Success(generateId()) else CheckoutResult.Failure("empty")
    }

    override fun close() {
        println("closed")
    }
}

private inline fun <reified T> Any.castOrNull(): T? = this as? T

fun String.toSlug(): String = lowercase().replace(' ', '-')

val Product.label: String
    get() = "$name (${price.cents})"

internal tailrec fun gcd(a: Int, b: Int): Int = if (b == 0) a else gcd(b, a % b)

fun main() {
    val (first, second) = Pair(1, 2)
    val catalogue = CachingCatalogue.create(RemoteCatalogue("https://example.com"))
    runBlocking {
        launch { catalogue.refresh() }
    }
    val anon = object : Runnable {
        override fun run() = println(first + second)
    }
    anon.run()
}
